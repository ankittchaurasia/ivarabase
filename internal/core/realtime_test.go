package core

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWSAcceptKey(t *testing.T) {
	// RFC 6455 section 1.3
	if got := wsAcceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatal(got)
	}
}

// echo server: every message comes back as text
func wsEchoServer(t *testing.T, max int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := wsUpgrade(w, r)
		if err != nil {
			return
		}
		if max > 0 {
			c.maxMsg = max
		}
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			c.WriteText(msg)
		}
	}))
}

type wsTestClient struct {
	c  net.Conn
	br *bufio.Reader
}

func wsDial(t *testing.T, srv *httptest.Server) *wsTestClient {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("handshake: %d %v", resp.StatusCode, resp.Header)
	}
	return &wsTestClient{c: c, br: br}
}

func (w *wsTestClient) frame(fin bool, op byte, payload []byte, masked bool) {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	hdr := []byte{b0, 0}
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
	body := append([]byte{}, payload...)
	if masked {
		hdr[1] |= 0x80
		mask := []byte{1, 2, 3, 4}
		hdr = append(hdr, mask...)
		for i := range body {
			body[i] ^= mask[i&3]
		}
	}
	w.c.Write(append(hdr, body...))
}

func (w *wsTestClient) read(t *testing.T) (byte, []byte) {
	t.Helper()
	var h [2]byte
	if _, err := io.ReadFull(w.br, h[:]); err != nil {
		t.Fatal(err)
	}
	if h[1]&0x80 != 0 {
		t.Fatal("server frames must not be masked")
	}
	n := int(h[1] & 0x7f)
	if n == 126 {
		var e [2]byte
		io.ReadFull(w.br, e[:])
		n = int(binary.BigEndian.Uint16(e[:]))
	} else if n == 127 {
		var e [8]byte
		io.ReadFull(w.br, e[:])
		n = int(binary.BigEndian.Uint64(e[:]))
	}
	p := make([]byte, n)
	io.ReadFull(w.br, p)
	return h[0] & 0x0f, p
}

func TestWSFragmentsAndPing(t *testing.T) {
	srv := wsEchoServer(t, 0)
	defer srv.Close()
	c := wsDial(t, srv)
	c.frame(false, wsOpText, []byte("hel"), true)
	c.frame(true, wsOpPing, []byte("p1"), true) // control frames may interleave
	c.frame(false, wsOpCont, []byte("lo "), true)
	c.frame(true, wsOpCont, []byte("world"), true)
	if op, p := c.read(t); op != wsOpPong || string(p) != "p1" {
		t.Fatalf("pong: %d %q", op, p)
	}
	if op, p := c.read(t); op != wsOpText || string(p) != "hello world" {
		t.Fatalf("echo: %d %q", op, p)
	}
	big := strings.Repeat("x", 70000) // 64-bit length on the way back
	c.frame(true, wsOpText, []byte(big), true)
	if _, p := c.read(t); string(p) != big {
		t.Fatalf("big echo: %d bytes", len(p))
	}
	c.frame(true, wsOpClose, binary.BigEndian.AppendUint16(nil, 4000), true)
	if op, p := c.read(t); op != wsOpClose || binary.BigEndian.Uint16(p) != 4000 {
		t.Fatalf("close echo: %d %v", op, p)
	}
}

func TestWSRejects(t *testing.T) {
	srv := wsEchoServer(t, 100)
	defer srv.Close()
	for name, tc := range map[string]struct {
		send func(c *wsTestClient)
		code int
	}{
		"unmasked": {func(c *wsTestClient) { c.frame(true, wsOpText, []byte("hi"), false) }, wsCloseProtocol},
		"too big":  {func(c *wsTestClient) { c.frame(true, wsOpText, make([]byte, 101), true) }, wsCloseTooBig},
		"too big frag": {func(c *wsTestClient) {
			c.frame(false, wsOpText, make([]byte, 60), true)
			c.frame(true, wsOpCont, make([]byte, 60), true)
		}, wsCloseTooBig},
		"bad utf8":    {func(c *wsTestClient) { c.frame(true, wsOpText, []byte{0xff, 0xfe}, true) }, wsCloseInvalidData},
		"orphan cont": {func(c *wsTestClient) { c.frame(true, wsOpCont, []byte("x"), true) }, wsCloseProtocol},
	} {
		c := wsDial(t, srv)
		tc.send(c)
		op, p := c.read(t)
		if op != wsOpClose || len(p) < 2 || int(binary.BigEndian.Uint16(p)) != tc.code {
			t.Errorf("%s: got op %d payload %v, want close %d", name, op, p, tc.code)
		}
		c.c.Close()
	}
}

func TestWSHandshakeErrors(t *testing.T) {
	srv := wsEchoServer(t, 0)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("plain GET: %d", resp.StatusCode)
	}
}

func TestRTFilter(t *testing.T) {
	rec := map[string]any{}
	json.Unmarshal([]byte(`{"id": 5, "room": "a", "score": 2.5, "ok": true, "gone": null}`), &rec)
	for f, want := range map[string]bool{
		"id=eq.5": true, "id=eq.6": false, "id=neq.6": true, "id=lt.10": true, "id=lte.5": true, "id=gt.5": false, "id=gte.5": true,
		"room=eq.a": true, "room=in.(b,a)": true, `room=in.("x","y")`: false, "score=gt.2": true, "ok=eq.true": true,
		"gone=eq.null": false, "gone=neq.x": false, "missing=eq.1": false, "id=in.(1,5)": true,
	} {
		pf, err := parseRTFilter(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if got := pf.match(rec); got != want {
			t.Errorf("%s: got %v want %v", f, got, want)
		}
	}
	for _, bad := range []string{"id", "id=like.a", "=eq.1", "id=in.1,2", "id=eq"} {
		if _, err := parseRTFilter(bad); err == nil {
			t.Errorf("%s: expected an error", bad)
		}
	}
}

func TestParsePGSubsEchoesBindings(t *testing.T) {
	subs, reply, err := parsePGSubs([]map[string]any{
		{"event": "*", "schema": "public"},
		{"event": "INSERT", "schema": "public", "table": "t", "filter": "id=eq.1"},
	})
	if err != nil || len(subs) != 2 {
		t.Fatal(err)
	}
	// supabase-js compares event/schema/table/filter exactly; absent keys must stay absent
	if _, ok := reply[0]["table"]; ok {
		t.Fatal("table must be omitted when the client didn't send one")
	}
	if reply[1]["filter"] != "id=eq.1" || reply[1]["id"] == reply[0]["id"] {
		t.Fatalf("%v", reply)
	}
	if _, _, err := parsePGSubs([]map[string]any{{"event": "TRUNCATE", "schema": "public"}}); err == nil {
		t.Fatal("unknown event accepted")
	}
}

func TestRTBucket(t *testing.T) {
	b := rtBucket{rate: 10, burst: 3}
	now := time.Now()
	n := 0
	for i := 0; i < 10; i++ {
		if b.allow(now) {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("burst: %d", n)
	}
	if !b.allow(now.Add(200 * time.Millisecond)) {
		t.Fatal("bucket should refill")
	}
}
