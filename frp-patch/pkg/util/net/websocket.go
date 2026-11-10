package net

import (
	"errors"
	"net"
	"net/http"
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
	muxer.HandleFunc(FrpWebsocketPath, func(w http.ResponseWriter, r *http.Request) {
		// Capture the real TCP peer address before the upgrade. For server-side
		// connections golang.org/x/net/websocket reports the WebSocket Origin
		// URL (e.g. "http://host") as RemoteAddr, which would hide the client
		// IP in the dashboard/API. Preserve the real address so frps reports the
		// true client IP for both ws:// and wss:// connections.
		realAddr := r.RemoteAddr

		websocket.Handler(func(c *websocket.Conn) {
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
		}).ServeHTTP(w, r)
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
