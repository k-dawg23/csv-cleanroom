package server

import (
	"bufio"
	"context"
	"csv-cleanroom/internal/jobs"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type rawServer struct {
	t       *testing.T
	addr    string
	manager *jobs.Manager
}
type rawConn struct {
	net.Conn
	r *bufio.Reader
}

func startRaw(t *testing.T) *rawServer {
	t.Helper()
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	m := jobs.New(nil)
	s := HTTPServer(m, l.Addr().String(), context.Background())
	done := make(chan struct{})
	go func() { _ = s.Serve(l); close(done) }()
	t.Cleanup(func() { _ = s.Close(); <-done; m.Close() })
	return &rawServer{t: t, addr: l.Addr().String(), manager: m}
}
func (s *rawServer) dial() *rawConn {
	s.t.Helper()
	c, e := net.DialTimeout("tcp4", s.addr, 3*time.Second)
	if e != nil {
		s.t.Fatal(e)
	}
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	s.t.Cleanup(func() { _ = c.Close() })
	return &rawConn{c, bufio.NewReader(c)}
}
func (c *rawConn) send(t *testing.T, wire string) {
	t.Helper()
	if _, e := io.WriteString(c, wire); e != nil {
		t.Fatal(e)
	}
}
func (c *rawConn) response(t *testing.T) (int, string, http.Header, bool) {
	t.Helper()
	r, e := http.ReadResponse(c.r, &http.Request{Method: "POST"})
	if e != nil {
		t.Fatalf("response: %v", e)
	}
	b, e := io.ReadAll(r.Body)
	if e != nil {
		t.Fatalf("body: %v", e)
	}
	_ = r.Body.Close()
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("unexpected CORS header %s", k)
		}
	}
	return r.StatusCode, string(b), r.Header, r.Close
}
func (s *rawServer) req(path, extra, body string) string {
	return fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\n%sContent-Length: %d\r\n\r\n%s", path, s.addr, s.addr, extra, len(body), body)
}
func rawMultipartBody() string {
	return "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x.csv\"\r\n\r\ncol\nsecretvalue\n\r\n--b--\r\n"
}
func (s *rawServer) upload(body string) string {
	return s.req("/api/upload", "Content-Type: multipart/form-data; boundary=b\r\n", body)
}
func (s *rawServer) fixture() string {
	s.t.Helper()
	c := s.dial()
	c.send(s.t, s.upload(rawMultipartBody()))
	n, b, _, _ := c.response(s.t)
	var v struct {
		Token string `json:"token"`
	}
	if n != 200 || json.Unmarshal([]byte(b), &v) != nil || v.Token == "" {
		s.t.Fatalf("fixture %d %s", n, b)
	}
	return v.Token
}
func (s *rawServer) process(tok string) string {
	return s.req("/api/process", "Content-Type: application/json\r\nX-CSV-Cleanroom-Token: "+tok+"\r\n", "{}")
}
func check(t *testing.T, c *rawConn, want int) {
	t.Helper()
	n, b, _, close := c.response(t)
	t.Logf("status=%d close=%v", n, close)
	if n != want {
		t.Errorf("got %d want %d body=%s", n, want, b)
	}
	if n >= 400 && (strings.Contains(b, "secretvalue") || strings.Contains(b, `"token"`)) {
		t.Error("sensitive data in error")
	}
}
func TestRawHost(t *testing.T) {
	s := startRaw(t)
	cases := []struct {
		name, host string
		want       int
	}{{"correct", s.addr, 404}, {"incorrect", "evil.test", 403}, {"port", "127.0.0.1:9", 403}, {"loopback", "localhost", 403}, {"suffix", s.addr + ".evil", 403}, {"whitespace", s.addr + " x", 400}, {"duplicate", s.addr + "\r\nHost: evil.test", 400}, {"missing", "", 400}, {"malformed", "[::1", 403}}
	for _, x := range cases {
		t.Run(x.name, func(t *testing.T) {
			c := s.dial()
			h := ""
			if x.host != "" {
				h = "Host: " + x.host + "\r\n"
			}
			c.send(t, "GET /missing HTTP/1.1\r\n"+h+"\r\n")
			check(t, c, x.want)
		})
	}
	t.Run("absolute_conflict", func(t *testing.T) {
		c := s.dial()
		c.send(t, "GET http://evil.test/missing HTTP/1.1\r\nHost: "+s.addr+"\r\n\r\n")
		check(t, c, 403)
	})
}
func TestRawOrigin(t *testing.T) {
	s := startRaw(t)
	cases := []struct {
		name, val string
		want      int
	}{{"correct", "http://" + s.addr, 200}, {"missing", "", 403}, {"port", "http://127.0.0.1:9", 403}, {"scheme", "https://" + s.addr, 403}, {"hostname", "http://localhost:8080", 403}, {"ip", "http://127.1:8080", 403}, {"suffix", "http://" + s.addr + ".evil", 403}, {"path", "http://" + s.addr + "/x", 403}, {"userinfo", "http://u@" + s.addr, 403}, {"malformed", "://", 403}, {"duplicate", "http://" + s.addr + "\r\nOrigin: http://" + s.addr, 403}, {"multiple", "http://" + s.addr + ", http://" + s.addr, 403}, {"whitespace", "http://" + s.addr + " x", 403}, {"null", "null", 403}, {"case", "HTTP://" + s.addr, 403}, {"outer_whitespace", " \t http://" + s.addr + " \t", 200}}
	for _, x := range cases {
		t.Run(x.name, func(t *testing.T) {
			c := s.dial()
			body := rawMultipartBody()
			origin := ""
			if x.val != "" {
				origin = "Origin: " + x.val + "\r\n"
			}
			c.send(t, fmt.Sprintf("POST /api/upload HTTP/1.1\r\nHost: %s\r\n%sContent-Type: multipart/form-data; boundary=b\r\nContent-Length: %d\r\n\r\n%s", s.addr, origin, len(body), body))
			check(t, c, x.want)
		})
	}
}
func TestRawHeaders(t *testing.T) {
	s := startRaw(t)
	prefix := "GET /missing HTTP/1.1\r\nHost: " + s.addr + "\r\nX-Pad: "
	suffix := "\r\n\r\n"
	mk := func(n int) string { return prefix + strings.Repeat("a", n-len(prefix)-len(suffix)) + suffix }
	cases := []struct {
		name, wire string
		want       int
	}{{"normal", mk(500), 404}, {"boundary_8192", mk(8192), 404}, {"boundary_8193", mk(8193), 431}, {"oversized", mk(16000), 431}, {"request_line", "GET /" + strings.Repeat("a", 10000) + " HTTP/1.1\r\nHost: " + s.addr + "\r\n\r\n", 431}, {"small_fields", "GET /missing HTTP/1.1\r\nHost: " + s.addr + "\r\n" + strings.Repeat("X-A: x\r\n", 1200) + "\r\n", 431}, {"large_value", mk(12000), 431}, {"repeated", "GET /missing HTTP/1.1\r\nHost: " + s.addr + "\r\n" + strings.Repeat("X-A: "+strings.Repeat("a", 100)+"\r\n", 90) + "\r\n", 431}, {"whitespace", "GET /missing HTTP/1.1\r\nHost: " + s.addr + "\r\nX-A: \t " + strings.Repeat(" ", 9000) + "x\r\n\r\n", 431}}
	for _, x := range cases {
		t.Run(x.name, func(t *testing.T) { c := s.dial(); c.send(t, x.wire); check(t, c, x.want) })
	}
}
func TestRawFraming(t *testing.T) {
	s := startRaw(t)
	tok := s.fixture()
	base := "POST /api/process HTTP/1.1\r\nHost: " + s.addr + "\r\nOrigin: http://" + s.addr + "\r\nContent-Type: application/json\r\nX-CSV-Cleanroom-Token: " + tok + "\r\n"
	cases := []struct {
		name, wire string
		want       int
		half       bool
	}{{"valid_length", base + "Content-Length: 2\r\n\r\n{}", 200, false}, {"missing_length", base + "\r\n", 400, false}, {"duplicate_length", base + "Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}", 200, false}, {"conflicting_length", base + "Content-Length: 2\r\nContent-Length: 3\r\n\r\n{}", 400, false}, {"invalid_length", base + "Content-Length: x\r\n\r\n", 400, false}, {"negative_length", base + "Content-Length: -1\r\n\r\n", 400, false}, {"short_body", base + "Content-Length: 3\r\n\r\n{}", 400, true}, {"chunked", base + "Transfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n\r\n", 200, false}, {"bad_chunk", base + "Transfer-Encoding: chunked\r\n\r\nx\r\n", 400, true}, {"truncated_chunk", base + "Transfer-Encoding: chunked\r\n\r\n2\r\n{", 400, true}, {"invalid_coding", base + "Transfer-Encoding: gzip\r\n\r\n", 501, false}, {"te_and_length", base + "Transfer-Encoding: chunked\r\nContent-Length: 99\r\n\r\n2\r\n{}\r\n0\r\n\r\n", 200, false}}
	for _, x := range cases {
		t.Run(x.name, func(t *testing.T) {
			c := s.dial()
			c.send(t, x.wire)
			if x.half {
				_ = c.Conn.(*net.TCPConn).CloseWrite()
			}
			check(t, c, x.want)
		})
	}
	t.Run("long_body", func(t *testing.T) {
		c := s.dial()
		c.send(t, base+"Content-Length: 2\r\n\r\n{}GARBAGE\r\n\r\n")
		check(t, c, 200)
		check(t, c, 400)
	})
	t.Run("after_chunk", func(t *testing.T) {
		c := s.dial()
		c.send(t, base+"Transfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n\r\nGARBAGE\r\n\r\n")
		check(t, c, 200)
		check(t, c, 400)
	})
}
func TestRawMultipart(t *testing.T) {
	s := startRaw(t)
	valid := rawMultipartBody()
	cases := []struct {
		name, body string
		want       int
	}{{"valid", valid, 200}, {"truncated_boundary", strings.TrimSuffix(valid, "--b--\r\n") + "--b-", 400}, {"missing_final", strings.TrimSuffix(valid, "\r\n--b--\r\n"), 400}, {"epilogue", valid + "epilogue", 200}, {"trailing_content", valid + "unexpected", 200}, {"envelope_excess", valid + strings.Repeat("x", maxBody+1-len(valid)), 413}}
	for _, x := range cases {
		t.Run(x.name, func(t *testing.T) {
			tok := s.fixture()
			c := s.dial()
			c.send(t, s.upload(x.body))
			check(t, c, x.want)
			if x.want != 200 {
				d := s.dial()
				d.send(t, s.process(tok))
				n, b, _, _ := d.response(t)
				if n != 200 || !strings.Contains(b, "secretvalue") {
					t.Fatalf("prior job lost: %d %s", n, b)
				}
			}
		})
	}
}
func TestRawReuseAndPreservation(t *testing.T) {
	s := startRaw(t)
	tok := s.fixture()
	c := s.dial()
	c.send(t, s.process(tok))
	check(t, c, 200)
	c.send(t, s.process(tok))
	check(t, c, 200)
	c.send(t, strings.Replace(s.process(tok), "Origin: http://"+s.addr, "Origin: null", 1))
	check(t, c, 403)
	c.send(t, s.process(tok))
	check(t, c, 200)
	c.send(t, strings.Replace(s.process(tok), "Host: "+s.addr, "Host: evil.test", 1))
	check(t, c, 403)
	c.send(t, s.process(tok))
	check(t, c, 200)
	bad := strings.Replace(s.upload(rawMultipartBody()), "Origin: http://"+s.addr, "Origin: null", 1)
	c.send(t, bad)
	check(t, c, 403)
	c.send(t, s.process(tok))
	check(t, c, 200)
	big := "GET /missing HTTP/1.1\r\nHost: " + s.addr + "\r\n" + strings.Repeat("X-A: "+strings.Repeat("a", 100)+"\r\n", 90) + "\r\n"
	c.send(t, big)
	n, b, _, closed := c.response(t)
	t.Logf("reused oversized status=%d close=%v", n, closed)
	if n != 413 && n != 431 {
		t.Errorf("oversized accepted: %d %s", n, b)
	}
	d := s.dial()
	d.send(t, s.process(tok))
	check(t, d, 200)
}

func TestRawOriginProtectedEndpoints(t *testing.T) {
	s := startRaw(t)
	tok := s.fixture()
	for _, path := range []string{"/api/process", "/api/download"} {
		t.Run(path, func(t *testing.T) {
			typ, body := "application/json", "{}"
			if path == "/api/download" {
				typ, body = "application/octet-stream", ""
			}
			c := s.dial()
			req := s.req(path, "Content-Type: "+typ+"\r\nX-CSV-Cleanroom-Token: "+tok+"\r\n", body)
			c.send(t, strings.Replace(req, "Origin: http://"+s.addr, "Origin: null", 1))
			check(t, c, 403)
			d := s.dial()
			d.send(t, s.process(tok))
			check(t, d, 200)
		})
	}
}

func TestRawChunkedEnvelopeLimit(t *testing.T) {
	s := startRaw(t)
	tok := s.fixture()
	body := rawMultipartBody() + strings.Repeat("x", maxBody+1-len(rawMultipartBody()))
	wire := fmt.Sprintf("POST /api/upload HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\nContent-Type: multipart/form-data; boundary=b\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", s.addr, s.addr, len(body), body)
	c := s.dial()
	c.send(t, wire)
	check(t, c, 413)
	d := s.dial()
	d.send(t, s.process(tok))
	n, b, _, _ := d.response(t)
	if n != 200 || !strings.Contains(b, "secretvalue") {
		t.Fatalf("prior job lost: %d %s", n, b)
	}
}
func TestRawFramingConflictReuse(t *testing.T) {
	s := startRaw(t)
	tok := s.fixture()
	c := s.dial()
	wire := fmt.Sprintf("POST /api/process HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\nContent-Type: application/json\r\nX-CSV-Cleanroom-Token: %s\r\nContent-Length: 99\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n\r\n", s.addr, s.addr, tok)
	c.send(t, wire)
	check(t, c, 200)
	c.send(t, s.process(tok))
	check(t, c, 200)
}

// Absolute-form must not allow an unauthorized wire Host to publish an upload.
func TestRawHostAbsoluteAuthorizedTargetConflictingHost(t *testing.T) {
	s := startRaw(t)
	tok := s.fixture()
	c := s.dial()
	body := rawMultipartBody()
	wire := fmt.Sprintf("POST http://%s/api/upload HTTP/1.1\r\nHost: evil.test\r\nOrigin: http://%s\r\nContent-Type: multipart/form-data; boundary=b\r\nContent-Length: %d\r\n\r\n%s", s.addr, s.addr, len(body), body)
	c.send(t, wire)
	check(t, c, 403)
	d := s.dial()
	d.send(t, s.process(tok))
	check(t, d, 200)
}
