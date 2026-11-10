package core

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// A small RFC 6455 WebSocket server for realtime. golang.org/x/net/websocket
// has no per-message size limit, no close codes and hides fragmentation, and
// the standard library has no WebSocket at all, so this file does the
// handshake and framing itself: client frames must be masked, fragmented
// messages are reassembled up to wsMaxMessage, pings are answered, and close
// frames are echoed with their code.

const (
	wsGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxMessage = 1 << 20 // 1 MB per (reassembled) message

	wsOpCont   = 0x0
	wsOpText   = 0x1
	wsOpBinary = 0x2
	wsOpClose  = 0x8
	wsOpPing   = 0x9
	wsOpPong   = 0xA

	wsCloseNormal      = 1000
	wsCloseGoingAway   = 1001
	wsCloseProtocol    = 1002
	wsCloseUnsupported = 1003
	wsCloseInvalidData = 1007
	wsClosePolicy      = 1008
	wsCloseTooBig      = 1009
	wsCloseInternal    = 1011
)

var errWSClosed = errors.New("websocket closed")

// wsCloseError is returned by ReadMessage when the peer closed (or the
// connection had to be closed for a protocol violation).
type wsCloseError struct {
	Code   int
	Reason string
}

func (e *wsCloseError) Error() string {
	return "websocket close " + strconv.Itoa(e.Code) + " " + e.Reason
}

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu       sync.Mutex
	closeOnce sync.Once
	closeSent bool
	maxMsg    int
}

// isWebSocketUpgrade reports whether r asks for a WebSocket.
func isWebSocketUpgrade(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

func wsAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsUpgrade validates the handshake, hijacks the connection and answers 101.
// On a bad handshake it writes an HTTP error and returns an error.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	fail := func(status int, msg string) (*wsConn, error) {
		supaError(w, status, msg, "")
		return nil, errors.New(msg)
	}
	if r.Method != http.MethodGet {
		return fail(405, "websocket handshake must be a GET")
	}
	if !isWebSocketUpgrade(r) {
		return fail(400, "expected a websocket upgrade (Connection: Upgrade, Upgrade: websocket)")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		return fail(426, "unsupported websocket version (need 13)")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if b, err := base64.StdEncoding.DecodeString(key); err != nil || len(b) != 16 {
		return fail(400, "invalid Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return fail(500, "connection does not support websocket upgrades")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Time{}) // the HTTP server's header timeout must not apply any more
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + wsAcceptKey(key) + "\r\n\r\n"
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetWriteDeadline(time.Time{})
	return &wsConn{conn: conn, br: brw.Reader, maxMsg: wsMaxMessage}, nil
}

func (c *wsConn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// ReadMessage returns the next complete text or binary message. Control
// frames are handled inline. A *wsCloseError means the connection is done.
func (c *wsConn) ReadMessage() (op byte, msg []byte, err error) {
	var buf []byte
	msgOp := byte(0)
	for {
		fin, fop, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch fop {
		case wsOpPing:
			if err := c.writeFrame(wsOpPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			code, reason := wsCloseNoStatus, ""
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload))
				reason = string(payload[2:])
			}
			reply := code
			if code == wsCloseNoStatus || !wsValidCloseCode(code) || !utf8.ValidString(reason) {
				reply = wsCloseProtocol
				if code == wsCloseNoStatus {
					reply = wsCloseNormal
				}
			}
			c.Close(reply, "")
			return 0, nil, &wsCloseError{Code: code, Reason: reason}
		case wsOpText, wsOpBinary:
			if msgOp != 0 {
				return 0, nil, c.fail(wsCloseProtocol, "new message inside a fragmented message")
			}
			msgOp = fop
		case wsOpCont:
			if msgOp == 0 {
				return 0, nil, c.fail(wsCloseProtocol, "continuation frame without a message")
			}
		default:
			return 0, nil, c.fail(wsCloseProtocol, "unknown opcode")
		}
		if len(buf)+len(payload) > c.maxMsg {
			return 0, nil, c.fail(wsCloseTooBig, "message too big")
		}
		buf = append(buf, payload...)
		if fin {
			if msgOp == wsOpText && !utf8.Valid(buf) {
				return 0, nil, c.fail(wsCloseInvalidData, "invalid UTF-8 in text message")
			}
			if buf == nil {
				buf = []byte{}
			}
			return msgOp, buf, nil
		}
	}
}

const wsCloseNoStatus = 1005

func wsValidCloseCode(code int) bool {
	switch {
	case code >= 1000 && code <= 1003, code >= 1007 && code <= 1011, code >= 3000 && code <= 4999:
		return true
	}
	return false
}

func (c *wsConn) fail(code int, reason string) error {
	c.Close(code, reason)
	return &wsCloseError{Code: code, Reason: reason}
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	op = h[0] & 0x0f
	if h[0]&0x70 != 0 {
		return false, 0, nil, c.fail(wsCloseProtocol, "reserved bits set")
	}
	if h[1]&0x80 == 0 {
		return false, 0, nil, c.fail(wsCloseProtocol, "client frames must be masked")
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if op >= 0x8 && (!fin || n > 125) {
		return false, 0, nil, c.fail(wsCloseProtocol, "invalid control frame")
	}
	if n > uint64(c.maxMsg) {
		return false, 0, nil, c.fail(wsCloseTooBig, "message too big")
	}
	var mask [4]byte
	if _, err = io.ReadFull(c.br, mask[:]); err != nil {
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i&3]
	}
	return fin, op, payload, nil
}

// writeFrame sends one unfragmented, unmasked frame (servers never mask).
func (c *wsConn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent {
		return errWSClosed
	}
	return c.writeFrameLocked(op, payload)
}

func (c *wsConn) writeFrameLocked(op byte, payload []byte) error {
	hdr := make([]byte, 2, 10)
	hdr[0] = 0x80 | op
	switch n := len(payload); {
	case n <= 125:
		hdr[1] = byte(n)
	case n <= 0xffff:
		hdr[1] = 126
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr[1] = 127
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.conn.Write(append(hdr, payload...)); err != nil {
		return err
	}
	return nil
}

// WriteText sends a text message.
func (c *wsConn) WriteText(b []byte) error { return c.writeFrame(wsOpText, b) }

// Close sends a close frame (once) and closes the TCP connection.
func (c *wsConn) Close(code int, reason string) {
	c.closeOnce.Do(func() {
		c.wmu.Lock()
		if !c.closeSent {
			if len(reason) > 120 {
				reason = reason[:120]
			}
			p := binary.BigEndian.AppendUint16(nil, uint16(code))
			c.writeFrameLocked(wsOpClose, append(p, reason...))
			c.closeSent = true
		}
		c.wmu.Unlock()
		c.conn.Close()
	})
}
