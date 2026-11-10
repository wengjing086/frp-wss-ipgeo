// Copyright 2026 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"errors"
	"net"
)

// wssListener is a channel-based net.Listener that accepts already-TLS-decrypted
// connections which still need a WebSocket upgrade (WebSocket Secure / WSS).
//
// frpc's "wss" protocol performs a TLS handshake first and then sends a
// "GET /~!frp" WebSocket upgrade request. The frps muxer routes the raw TLS
// handshake (first byte 0x16) to the TLS listener, where CheckAndEnableTLS
// negotiates TLS. After that, HandleListener detects the trailing WebSocket
// upgrade and hands the connection off to this listener. The WebsocketListener
// wrapping it then performs the upgrade, producing a websocket.Conn that is
// handled like any other frp control connection (yamux + frp messages).
//
// This removes the previous limitation where frps could only accept plain
// WebSocket (ws://) directly and needed an external TLS-terminating proxy
// (nginx / https2http plugin) in front of it for WSS.
type wssListener struct {
	acceptCh chan net.Conn
	addr     net.Addr
}

func newWSSListener() *wssListener {
	return &wssListener{
		acceptCh: make(chan net.Conn),
		addr:     &net.TCPAddr{IP: net.IPv4zero, Port: 0},
	}
}

func (l *wssListener) Accept() (net.Conn, error) {
	c, ok := <-l.acceptCh
	if !ok {
		return nil, errors.New("wss listener closed")
	}
	return c, nil
}

func (l *wssListener) Close() error {
	close(l.acceptCh)
	return nil
}

func (l *wssListener) Addr() net.Addr {
	return l.addr
}

// peekConn wraps a net.Conn so that the bytes in buf are served first on Read,
// after which reads are delegated to the underlying connection. It is used to
// transparently inspect (and replay) the WebSocket upgrade request that follows
// a TLS handshake in frpc's "wss" protocol, without losing any bytes of the
// stream handed to the WebSocket upgrader or the frp message reader.
type peekConn struct {
	net.Conn
	buf *bytes.Reader
}

func (p *peekConn) Read(b []byte) (int, error) {
	if p.buf.Len() > 0 {
		return p.buf.Read(b)
	}
	return p.Conn.Read(b)
}
