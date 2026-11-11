package net

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

var ErrWebsocketListenerClosed = errors.New("websocket listener closed")

const (
	FrpWebsocketPath = "/~!frp"
)

// wsRealAddr is the real TCP peer address captured from the HTTP upgrade
// request, exposed as a net.Addr so it can be returned by RemoteAddr().
type wsRealAddr string

func (a wsRealAddr) Network() string { return "tcp" }

func (a wsRealAddr) String() string { return string(a) }

// wsRealAddrConn wraps a *websocket.Conn so that RemoteAddr() reports the real
// underlying TCP peer address instead of the WebSocket Origin URL that
// golang.org/x/net/websocket returns for server-side connections
// (see websocket.Conn.RemoteAddr: it returns &Addr{ws.config.Origin}).
// Without this the frps dashboard/API would show e.g. "http://127.0.0.1"
// instead of the actual client IP for ws:// and wss:// clients.
type wsRealAddrConn struct {
	*websocket.Conn
	realAddr net.Addr
}

func (c *wsRealAddrConn) RemoteAddr() net.Addr {
	return c.realAddr
}

type WebsocketListener struct {
	ln       net.Listener
	acceptCh chan net.Conn
	done     chan struct{}

	server *http.Server

	closeOnce sync.Once
}

// NewWebsocketListener to handle websocket connections
// ln: tcp listener for websocket connections
func NewWebsocketListener(ln net.Listener) (wl *WebsocketListener) {
	wl = &WebsocketListener{
		ln:       ln,
		acceptCh: make(chan net.Conn),
		done:     make(chan struct{}),
	}

	muxer := http.NewServeMux()
	// 根路径兜住**所有**路径，而不是只注册 `/~!frp`。
	//
	// 原因（CDN 场景实测）：阿里云 ESA / Cloudflare 这类网关的 WAF 会把
	// `/~!frp` 判为可疑路径直接回 403；用户即便把 frpc 的
	// `transport.websocket.path` 改成 `/ws`，服务端也只认 `/~!frp`，
	// 于是握手永远失败。路径本身对 frp 没有任何语义，只是 HTTP 层的一个
	// 标识，所以这里改成「任意路径 + `Upgrade: websocket`」即升级。
	//
	// 同时还多承担一件事：非升级的普通 HTTP 请求（CDN 回源健康检查
	// `GET /` / `HEAD /`）要回一个正常的 200，否则网关会把源站标记为
	// 不健康、对外返回 520 Origin Error。
	muxer.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !isWebsocketRequest(r) {
			writeHTTPFallback(w, r)
			return
		}
		// Capture the real TCP peer address before the upgrade. For server-side
		// connections golang.org/x/net/websocket reports the WebSocket Origin
		// URL (e.g. "http://host") as RemoteAddr, which would hide the client
		// IP in the dashboard/API. Preserve the real address so frps reports the
		// true client IP for both ws:// and wss:// connections.
		realAddr := r.RemoteAddr

		// 用 websocket.Server 而不是 websocket.Handler，只为替换掉默认的
		// Origin 校验（默认实现 checkOrigin：请求没带 `Origin` 头就直接
		// 403 "null origin"）。
		//
		// 为什么必须放开：frp 的控制连接不是浏览器发起的，`Origin` 对它
		// 没有任何安全意义；而**不发 Origin 的客户端非常常见** ——
		// 官方 Go frpc 之外的自研客户端、本项目 Rust 版 frpc、以及部分
		// 反代/网关在回源时会剥掉该头。保留默认校验的结果就是"握手一律
		// 403"，且错误信息里看不出是 Origin 的问题，极难排查。
		websocket.Server{
			Handshake: func(_ *websocket.Config, _ *http.Request) error { return nil },
			Handler: func(c *websocket.Conn) {
				// The tunnel payload is a raw byte stream (yamux), not UTF-8 text.
				// Send it as binary frames; otherwise RFC 6455-compliant intermediaries
				// (e.g. API gateways/reverse proxies) UTF-8-validate the default text
				// frames and close the connection on invalid bytes.
				c.PayloadType = websocket.BinaryFrame

				wsConn := &wsRealAddrConn{Conn: c, realAddr: wsRealAddr(realAddr)}

				notifyCh := make(chan struct{})
				conn := WrapCloseNotifyConn(wsConn, func(_ error) {
					close(notifyCh)
				})
				select {
				case <-wl.done:
					conn.Close()
					return
				case wl.acceptCh <- conn:
					// A ready handoff may win when Close races with it. Once delivered,
					// the connection is owned by the caller of Accept.
				}
				<-notifyCh
			},
		}.ServeHTTP(w, r)
	})

	wl.server = &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           muxer,
		ReadHeaderTimeout: 60 * time.Second,
	}

	go func() {
		_ = wl.server.Serve(ln)
	}()
	return
}

func (p *WebsocketListener) Accept() (net.Conn, error) {
	select {
	case <-p.done:
		return nil, ErrWebsocketListenerClosed
	default:
	}
	select {
	case <-p.done:
		return nil, ErrWebsocketListenerClosed
	case c := <-p.acceptCh:
		return c, nil
	}
}

func (p *WebsocketListener) Close() error {
	p.closeOnce.Do(func() { close(p.done) })
	return p.server.Close()
}

func (p *WebsocketListener) Addr() net.Addr {
	return p.ln.Addr()
}

// ---------------------------------------------------------------------------
// HTTP 探测 / 兜底响应（CDN 场景必需）
// ---------------------------------------------------------------------------

const (
	// maxHTTPHeadBytes 是请求头上限，防「只发半截头不放行」的慢速攻击。
	maxHTTPHeadBytes = 16 * 1024

	// healthPathRoot 是各家 CDN 回源健康检查默认打的路径。
	// 另加 `/health*`、`/status*` 前缀，方便自定义探测。
	healthPathRoot = "/"
)

var errNotHTTP = errors.New("not an HTTP request")
var errHeadTooLarge = errors.New("http head too large")

// isWebsocketRequest 判断一个已经解析好的 HTTP 请求是否是 WebSocket 升级。
func isWebsocketRequest(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

// writeHTTPFallback 回应**非升级**的普通 HTTP 请求。
//
// 这是 CDN 能用的前提：网关回源时会先打一个健康检查（`GET /`），
// 源站必须回一个完整、可解析的 HTTP 响应并正常关闭，否则边缘节点判定
// 源站不健康，对外固定返回 520 Origin Error —— 表现为「客户端拿到 520，
//
//	但 frps 日志里什么都没有」。
func writeHTTPFallback(w http.ResponseWriter, r *http.Request) {
	healthy := r.URL.Path == healthPathRoot ||
		strings.HasPrefix(r.URL.Path, "/health") ||
		strings.HasPrefix(r.URL.Path, "/status")

	code := http.StatusNotFound
	body := "not found\n"
	if healthy {
		code = http.StatusOK
		body = "ok\n"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Connection", "close")
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, body)
	}
}

// looksLikeHTTPMethod 只看前 4 个字节就能判断「像不像 HTTP 请求行」。
//
// 必须是**零等待**的判断：正常 frp over TLS 的控制连接首字节是二进制，
// 如果这里不退出去、反而死等 `\r\n\r\n`，每条控制连接都会白等一个超时。
func looksLikeHTTPMethod(b []byte) bool {
	if len(b) < 4 {
		return true
	}
	switch string(b[:4]) {
	case "GET ", "HEAD", "POST", "PUT ", "OPTI", "DELE", "PATC", "CONN", "TRAC":
		return true
	}
	return false
}

// IsHTTPRequest 判断探测到的字节是否是一条完整的 HTTP 请求行。
func IsHTTPRequest(head []byte) bool {
	line, _, _ := bytes.Cut(head, []byte("\r\n"))
	return bytes.Contains(line, []byte(" HTTP/1."))
}

// IsWebsocketUpgrade 判断探测到的字节是否是 WebSocket 升级请求。
//
// 不看路径：路径只是 HTTP 层的标识，frp 本身不用它，而 CDN/WAF 恰恰会
// 拿 `/~!frp` 当特征去拦。改由 `Upgrade` 头判定，换任何路径都能用。
func IsWebsocketUpgrade(head []byte) bool {
	if !IsHTTPRequest(head) {
		return false
	}
	for _, line := range bytes.Split(head, []byte("\r\n"))[1:] {
		k, v, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		if bytes.EqualFold(bytes.TrimSpace(k), []byte("upgrade")) &&
			bytes.EqualFold(bytes.TrimSpace(v), []byte("websocket")) {
			return true
		}
	}
	return false
}

// ReadHTTPHead 在一条（已解密的）连接上读出一个完整的 HTTP 请求头。
//
// 返回的字节是**读到的全部内容**，调用方必须原样回放 —— 正常 frp 连接
// 也会走这里（前 4 字节不是 HTTP 方法就立刻返回），回放少了就会破坏协议。
//
// 三种结局：
//   - 读到 `\r\n\r\n`：err 为 nil，返回完整头；
//   - 前几字节就不像 HTTP：err 为 errNotHTTP（**立即**返回，不等待）；
//   - 超时 / 头过大：err 非 nil，buf 里是已读字节，调用方照样回放。
func ReadHTTPHead(c net.Conn, timeout time.Duration) ([]byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()

	buf := make([]byte, 0, 512)
	chunk := make([]byte, 512)
	for {
		if i := bytes.Index(buf, []byte("\r\n\r\n")); i >= 0 {
			return buf, nil
		}
		if len(buf) > maxHTTPHeadBytes {
			return buf, errHeadTooLarge
		}
		if !looksLikeHTTPMethod(buf) {
			return buf, errNotHTTP
		}
		n, err := c.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			continue
		}
		if err != nil {
			return buf, err
		}
	}
}

// WriteHTTPFallbackToConn 在裸连接（TLS 已解密）上回应普通 HTTP 请求。
//
// 与 writeHTTPFallback 的区别：这里没有 `http.ResponseWriter`，是直接在
// 握手后的字节流上写，写完调用方负责 `Close()`（TLS 连接会因此发出
// close_notify，少了这一步网关看到的是「连接被重置」）。
func WriteHTTPFallbackToConn(c net.Conn, head []byte) {
	line, _, _ := bytes.Cut(head, []byte("\r\n"))
	fields := bytes.Fields(line)
	path := "/"
	method := ""
	if len(fields) > 0 {
		method = string(fields[0])
	}
	if len(fields) > 1 {
		path = string(fields[1])
	}
	healthy := path == healthPathRoot ||
		strings.HasPrefix(path, "/health") ||
		strings.HasPrefix(path, "/status")
	code, reason, body := http.StatusNotFound, "Not Found", "not found\n"
	if healthy {
		code, reason, body = http.StatusOK, "OK", "ok\n"
	}
	resp := fmt.Sprintf(
		"HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\n"+
			"Content-Length: %d\r\nConnection: close\r\n\r\n",
		code, reason, len(body))
	if method == http.MethodHead {
		resp = fmt.Sprintf(
			"HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\n"+
				"Content-Length: %d\r\nConnection: close\r\n\r\n",
			code, reason, len(body))
		_, _ = c.Write([]byte(resp))
		return
	}
	_, _ = c.Write(append([]byte(resp), body...))
}

