package server

import (
	"bufio"
	"context"
	"csv-cleanroom/internal/jobs"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func v2ObserveTemp(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("TMP", dir)
	if os.TempDir() != dir {
		t.Fatalf("temporary directory not selected: %s", os.TempDir())
	}
	t.Cleanup(func() {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Errorf("observed TMPDIR contents: %v, %v", entries, err)
		}
	})
}
func v2WaitNotBusy(t *testing.T, s *rawServer, token string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c := s.dial()
		c.send(t, s.process(token))
		n, b, _, _ := c.response(t)
		_ = c.Close()
		if n == 200 {
			return b
		}
		if n != 409 {
			t.Fatalf("process: %d %s", n, b)
		}
	}
	t.Fatal("admission remained busy")
	return ""
}
func v2WaitBusy(t *testing.T, s *rawServer, token string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c := s.dial()
		c.send(t, s.process(token))
		n, b, _, _ := c.response(t)
		_ = c.Close()
		if n == 409 {
			return
		}
		if n != 200 {
			t.Fatalf("probe %d %s", n, b)
		}
	}
	t.Fatal("request never acquired admission")
}
func v2Download(s *rawServer, token string) string {
	return s.req("/api/download", "Content-Type: application/octet-stream\r\nX-CSV-Cleanroom-Token: "+token+"\r\n", "")
}
func v2Snapshot(t *testing.T, s *rawServer, token string) string {
	t.Helper()
	c := s.dial()
	c.send(t, v2Download(s, token))
	n, b, _, _ := c.response(t)
	if n != 200 {
		t.Fatalf("download %d %s", n, b)
	}
	return b
}
func v2WaitDownload(t *testing.T, s *rawServer, token string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c := s.dial()
		c.send(t, v2Download(s, token))
		n, b, _, _ := c.response(t)
		_ = c.Close()
		if n == 200 {
			return b
		}
		if n != 409 {
			t.Fatalf("download: %d %s", n, b)
		}
	}
	t.Fatal("download remained busy")
	return ""
}
func v2Published(t *testing.T, m *jobs.Manager, token string) *jobs.Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lease *jobs.Lease
	for {
		var err error
		lease, err = m.Acquire(false, token)
		if err == nil {
			break
		}
		if !errors.Is(err, jobs.ErrBusy) || !time.Now().Before(deadline) {
			t.Fatalf("acquire snapshot: %v", err)
		}
		runtime.Gosched()
	}
	snapshot := lease.Job.Snapshot
	lease.End(false)
	if snapshot == nil {
		t.Fatal("missing snapshot")
	}
	return snapshot
}

// The upload handler reads its body only after acquiring the admission lease.
type v2ObservedBody struct {
	io.ReadCloser
	once    sync.Once
	reading chan struct{}
}

func (b *v2ObservedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.reading) })
	return b.ReadCloser.Read(p)
}

func TestV2UploadDisconnect(t *testing.T) {
	v2ObserveTemp(t)
	for _, part := range []struct{ name, body string }{{"part_headers", "--b\r\nContent-Disposition: form-data; name=\"file\""}, {"csv_data", "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x.csv\"\r\n\r\ncol\nincomplete"}, {"near_end", strings.TrimSuffix(rawMultipartBody(), "--b--\r\n") + "--b-"}} {
		t.Run(part.name, func(t *testing.T) {
			reading := make(chan struct{})
			s := startRawWithHandler(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("X-Test-Observe-Upload") == "yes" {
						r.Body = &v2ObservedBody{ReadCloser: r.Body, reading: reading}
					}
					next.ServeHTTP(w, r)
				})
			})
			tok := s.fixture()
			v2WaitNotBusy(t, s, tok)
			before := v2Snapshot(t, s, tok)
			published := v2Published(t, s.manager, tok)
			c := s.dial()
			c.send(t, fmt.Sprintf("POST /api/upload HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\nX-Test-Observe-Upload: yes\r\nContent-Type: multipart/form-data; boundary=b\r\nContent-Length: %d\r\n\r\n%s", s.addr, s.addr, len(part.body)+100, part.body))
			response := make(chan string, 1)
			go func() {
				r, err := http.ReadResponse(c.r, &http.Request{Method: "POST"})
				if err != nil {
					response <- err.Error()
					return
				}
				defer r.Body.Close()
				response <- r.Status
			}()
			select {
			case <-reading:
			case result := <-response:
				t.Fatalf("upload rejected or ended before admission: %s", result)
			case <-time.After(5 * time.Second):
				t.Fatal("upload did not begin reading after admission")
			}
			// Once the upload is admitted, verify that another operation is refused.
			v2WaitBusy(t, s, tok)
			_ = c.Close()
			select {
			case <-response:
			case <-time.After(5 * time.Second):
				t.Fatal("upload response reader did not exit after disconnect")
			}
			if got := v2WaitDownload(t, s, tok); got != before {
				t.Fatalf("prior snapshot changed: %q", got)
			}
			if v2Published(t, s.manager, tok) != published {
				t.Fatal("published snapshot and report changed")
			}
			after := v2WaitNotBusy(t, s, tok)
			if !strings.Contains(after, "secretvalue") {
				t.Fatal("original preview lost")
			}
			if got := v2Snapshot(t, s, tok); got != before {
				t.Fatalf("snapshot changed: %q", got)
			}
			t.Log("admitted upload disconnected; previous token, preview and download preserved")
		})
	}
}
func TestV2ProcessDisconnect(t *testing.T) {
	v2ObserveTemp(t)
	s := startRaw(t)
	tok := s.fixture()
	original := v2WaitNotBusy(t, s, tok)
	before := v2Snapshot(t, s, tok)
	published := v2Published(t, s.manager, tok)
	c := s.dial()
	c.send(t, fmt.Sprintf("POST /api/process HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\nContent-Type: application/json\r\nX-CSV-Cleanroom-Token: %s\r\nContent-Length: 80\r\n\r\n{\"trimWhitespace\":", s.addr, s.addr, tok))
	v2WaitBusy(t, s, tok)
	_ = c.Close()
	if got := v2WaitDownload(t, s, tok); got != before {
		t.Fatal("prior snapshot changed")
	}
	if v2Published(t, s.manager, tok) != published {
		t.Fatal("published snapshot and report changed")
	}
	got := v2WaitNotBusy(t, s, tok)
	if got != original || v2Snapshot(t, s, tok) != before {
		t.Fatal("snapshot or report changed")
	}
	t.Log("disconnect observed during admitted request body; later process succeeded")
}
func TestV2ProcessPrecancel(t *testing.T) {
	v2ObserveTemp(t)
	m := jobs.New(nil)
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	tok, w := upload(t, s, "a\n x \n")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request(s, "/api/process", "application/json", tok, strings.NewReader("{}"))
	before := w.Body.String()
	previous := request(s, "/api/download", "application/octet-stream", tok, strings.NewReader("")).Body.String()
	published := v2Published(t, m, tok)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := originRequest("POST", "http://127.0.0.1:8080/api/process", strings.NewReader(`{"trimWhitespace":true}`)).WithContext(ctx)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSV-Cleanroom-Token", tok)
	s.ServeHTTP(httptest.NewRecorder(), r)
	download := request(s, "/api/download", "application/octet-stream", tok, strings.NewReader(""))
	if download.Code != 200 || download.Body.String() != previous {
		t.Fatal("pre-cancelled candidate changed snapshot")
	}
	if v2Published(t, m, tok) != published {
		t.Fatal("pre-cancelled candidate changed snapshot and report")
	}
	w = request(s, "/api/process", "application/json", tok, strings.NewReader("{}"))
	if w.Code != 200 || w.Body.String() != before {
		t.Fatal("failed pre-cancel processing changed subsequent result")
	}
	t.Log("pre-cancelled handler did not publish; gate released")
}

type v2Listener struct {
	net.Listener
	on      atomic.Bool
	first   chan struct{}
	release chan struct{}
	failure chan error
	once    sync.Once
}
type v2Conn struct {
	net.Conn
	l      *v2Listener
	writes int
}

func (l *v2Listener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	return &v2Conn{Conn: c, l: l}, nil
}
func (c *v2Conn) Write(p []byte) (int, error) {
	if c.l.on.Load() {
		c.writes++
		if c.writes == 2 {
			select {
			case <-c.l.release:
			case <-time.After(5 * time.Second):
				return 0, errors.New("test barrier timed out")
			}
		}
	}
	n, e := c.Conn.Write(p)
	if c.l.on.Load() && c.writes == 1 {
		c.l.once.Do(func() { close(c.l.first) })
	}
	if e != nil && c.l.on.Load() {
		select {
		case c.l.failure <- e:
		default:
		}
	}
	return n, e
}
func TestV2DownloadDisconnect(t *testing.T) {
	v2ObserveTemp(t)
	l0, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	l := &v2Listener{Listener: l0, first: make(chan struct{}), release: make(chan struct{}), failure: make(chan error, 1)}
	m := jobs.New(nil)
	srv := HTTPServer(m, l.Addr().String(), context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(l); close(done) }()
	t.Cleanup(func() { _ = srv.Close(); <-done; m.Close() })
	s := &rawServer{t: t, addr: l.Addr().String(), manager: m}
	csv := "col\n" + strings.Repeat("x", 2*1024*1024) + "\n"
	body := strings.Replace(rawMultipartBody(), "col\nsecretvalue\n", csv, 1)
	c := s.dial()
	c.send(t, s.upload(body))
	n, b, _, _ := c.response(t)
	var v struct {
		Token string `json:"token"`
	}
	if n != 200 || json.Unmarshal([]byte(b), &v) != nil || v.Token == "" {
		t.Fatalf("upload %d", n)
	}
	v2WaitNotBusy(t, s, v.Token)
	c = s.dial()
	l.on.Store(true)
	c.send(t, v2Download(s, v.Token))
	select {
	case <-l.first:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not write")
	}
	r, e := http.ReadResponse(bufio.NewReader(c.Conn), &http.Request{Method: "POST"})
	if e != nil {
		t.Fatal(e)
	}
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "text/csv; charset=utf-8" || r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment;") || r.ContentLength < 1000000 {
		t.Fatalf("headers: %v length %d", r.Header, r.ContentLength)
	}
	var one [1]byte
	if _, e = io.ReadFull(r.Body, one[:]); e != nil {
		t.Fatal(e)
	}
	_ = c.Conn.(*net.TCPConn).SetLinger(0)
	_ = c.Close()
	close(l.release)
	select {
	case e := <-l.failure:
		t.Logf("server observed TCP write failure: %v; client read one byte of %d", e, r.ContentLength)
	case <-time.After(5 * time.Second):
		t.Fatal("server write failure not observed")
	}
	l.on.Store(false)
	out := v2WaitDownload(t, s, v.Token)
	if out != csv {
		t.Fatal("download snapshot changed")
	}
	result := v2WaitNotBusy(t, s, v.Token)
	if !strings.Contains(result, "columnCount") {
		t.Fatal("job inconsistent")
	}
	out = v2Snapshot(t, s, v.Token)
	if out != csv {
		t.Fatal("download snapshot changed")
	}
}

func TestV2FailedDownloadDoesNotRefresh(t *testing.T) {
	now := time.Now()
	m := jobs.New(func() time.Time { return now })
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	tok, w := upload(t, s, "a\nx\n")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request(s, "/api/process", "application/json", tok, strings.NewReader("{}"))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	now = now.Add(10 * time.Minute)
	r := originRequest("POST", "http://127.0.0.1:8080/api/download", nil)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("X-CSV-Cleanroom-Token", tok)
	bad := &badWriter{header: http.Header{}}
	s.ServeHTTP(bad, r)
	if bad.calls != 1 {
		t.Fatal("write failure was not exercised")
	}
	now = now.Add(5 * time.Minute)
	if _, err := m.Acquire(false, tok); !errors.Is(err, jobs.ErrInvalid) {
		t.Fatalf("failed download refreshed expiry: %v", err)
	}
}
