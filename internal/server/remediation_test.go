package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"csv-cleanroom/internal/jobs"
)

func rawUpload(s *Server, body []byte) *httptest.ResponseRecorder {
	return request(s, "/api/upload", "multipart/form-data; boundary=b", "", bytes.NewReader(body))
}
func multipartBody(disposition, content string) []byte {
	return []byte("--b\r\nContent-Disposition: " + disposition + "\r\n\r\n" + content + "\r\n--b--\r\n")
}
func TestMultipartMetadata(t *testing.T) {
	cases := []struct {
		name, disp string
		body       []byte
		code       int
	}{
		{name: "valid", disp: `form-data; name="file"; filename="x.csv"`, code: 200},
		{name: "missing", body: []byte("--b--\r\n"), code: 400},
		{name: "field", disp: `form-data; name="other"; filename="x.csv"`, code: 400},
		{name: "malformed", disp: `form-data; name="file"; filename="x`, code: 400},
		{name: "duplicate", disp: `form-data; name="file"; filename="x"; filename="x"`, code: 400},
		{name: "bad_utf8", disp: "form-data; name=\"file\"; filename=\"\xff\"", code: 400},
		{name: "filename_255", disp: `form-data; name="file"; filename="` + strings.Repeat("a", 255) + `"`, code: 200},
		{name: "filename_256", disp: `form-data; name="file"; filename="` + strings.Repeat("a", 256) + `"`, code: 400},
		{name: "bad_boundary", body: []byte("--c\r\n"), code: 400},
		{name: "truncated", body: []byte("--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x\"\r\n\r\nh\n"), code: 400},
		{name: "two_parts", body: []byte("--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x\"\r\n\r\nh\n\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"y\"\r\n\r\nh\n\r\n--b--\r\n"), code: 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := jobs.New(nil)
			defer m.Close()
			s := New(m, "127.0.0.1:8080")
			b := tc.body
			if b == nil {
				b = multipartBody(tc.disp, "h\nx\n")
			}
			w := rawUpload(s, b)
			if w.Code != tc.code {
				t.Fatalf("got %d %s", w.Code, w.Body)
			}
		})
	}
}
func TestMultipartSizeBoundaries(t *testing.T) {
	disp := `form-data; name="file"; filename="x.csv"`
	for _, tc := range []struct {
		name string
		body []byte
		code int
	}{
		{"file_exact", multipartBody(disp, "h\n"+strings.Repeat("x", 10*1024*1024-2)), 200},
		{"file_over", multipartBody(disp, "h\n"+strings.Repeat("x", 10*1024*1024-1)), 413},
		{"body_exact", func() []byte {
			b := multipartBody(disp, "h\nx\n")
			return append(b, bytes.Repeat([]byte{'x'}, maxBody-len(b))...)
		}(), 200},
		{"body_over", func() []byte {
			b := multipartBody(disp, "h\nx\n")
			return append(b, bytes.Repeat([]byte{'x'}, maxBody-len(b)+1)...)
		}(), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := jobs.New(nil)
			defer m.Close()
			w := rawUpload(New(m, "127.0.0.1:8080"), tc.body)
			if w.Code != tc.code {
				t.Fatalf("%d: %s", w.Code, w.Body)
			}
		})
	}
	for _, n := range []int{4096, 4097} {
		t.Run(fmt.Sprint("part_headers_", n), func(t *testing.T) {
			m := jobs.New(nil)
			defer m.Close()
			base := "Content-Disposition: " + disp + "\r\nContent-Type: "
			end := "\r\n\r\n"
			body := []byte("--b\r\n" + base + strings.Repeat("x", n-len(base)-len(end)) + end + "h\nx\n\r\n--b--\r\n")
			w := rawUpload(New(m, "127.0.0.1:8080"), body)
			want := 200
			if n > 4096 {
				want = 413
			}
			if w.Code != want {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("secret reader detail") }
func TestLimitErrorTracking(t *testing.T) {
	m := jobs.New(nil)
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	w := request(s, "/api/upload", "multipart/form-data; boundary=b", "", io.MultiReader(strings.NewReader("--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x\"\r\n\r\n"), errorReader{}))
	if w.Code != 400 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal(w.Code, w.Body)
	}
	tr := &limitTracker{r: http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(strings.NewReader("12")), 1)}
	_, _ = io.ReadAll(tr)
	if !tr.exceeded {
		t.Fatal("max error not tracked")
	}
	w = httptest.NewRecorder()
	uploadError(w, tr, csvReadErrorForTest)
	if w.Code != 413 {
		t.Fatal(w.Code)
	}
}

var csvReadErrorForTest = errors.New("safe error")

type badWriter struct {
	header http.Header
	calls  int
}

func (w *badWriter) Header() http.Header       { return w.header }
func (w *badWriter) WriteHeader(int)           {}
func (w *badWriter) Write([]byte) (int, error) { w.calls++; return 0, errors.New("write failed") }
func TestDownloadFailureReleasesAdmission(t *testing.T) {
	m := jobs.New(nil)
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
	r := originRequest("POST", "http://127.0.0.1:8080/api/download", nil)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("X-CSV-Cleanroom-Token", tok)
	bad := &badWriter{header: http.Header{}}
	s.ServeHTTP(bad, r)
	if bad.calls != 1 {
		t.Fatal(bad.calls)
	}
	w = request(s, "/api/download", "application/octet-stream", tok, strings.NewReader(""))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
func TestCancellationReleasesAdmission(t *testing.T) {
	m := jobs.New(nil)
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := originRequest("POST", "http://127.0.0.1:8080/api/upload", bytes.NewReader(multipartBody(`form-data; name="file"; filename="x"`, "a\nx\n"))).WithContext(ctx)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	s.ServeHTTP(httptest.NewRecorder(), r)
	_, w := upload(t, s, "a\nx\n")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
func TestExpiryActiveLease(t *testing.T) {
	now := time.Now()
	m := jobs.New(func() time.Time { return now })
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	tok, w := upload(t, s, "a\nx\n")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	l, e := m.Acquire(false, tok)
	if e != nil {
		t.Fatal(e)
	}
	now = now.Add(jobs.Lifetime)
	if _, e = m.Acquire(false, tok); !errors.Is(e, jobs.ErrBusy) {
		t.Fatal(e)
	}
	l.End(true)
	if l, e = m.Acquire(false, tok); e != nil {
		t.Fatal(e)
	}
	l.End(false)
}

type blockedReader struct {
	started chan struct{}
	release chan struct{}
	body    io.Reader
	once    bool
}

func (b *blockedReader) Read(p []byte) (int, error) {
	if !b.once {
		b.once = true
		close(b.started)
		<-b.release
	}
	return b.body.Read(p)
}

type blockedWriter struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return w.ResponseRecorder.Write(p)
}
func TestConcurrentOperationAdmission(t *testing.T) {
	for _, active := range []string{"upload", "process", "download"} {
		t.Run(active, func(t *testing.T) {
			m := jobs.New(nil)
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
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var r *http.Request
			var writer http.ResponseWriter = httptest.NewRecorder()
			switch active {
			case "upload":
				r = originRequest("POST", "http://127.0.0.1:8080/api/upload", &blockedReader{started: started, release: release, body: bytes.NewReader(multipartBody(`form-data; name="file"; filename="x"`, "a\nx\n"))})
				r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
			case "process":
				r = originRequest("POST", "http://127.0.0.1:8080/api/process", &blockedReader{started: started, release: release, body: strings.NewReader("{}")})
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-CSV-Cleanroom-Token", tok)
			case "download":
				r = originRequest("POST", "http://127.0.0.1:8080/api/download", nil)
				r.Header.Set("Content-Type", "application/octet-stream")
				r.Header.Set("X-CSV-Cleanroom-Token", tok)
				writer = &blockedWriter{ResponseRecorder: httptest.NewRecorder(), started: started, release: release}
			}
			r.Header.Set("Origin", "http://127.0.0.1:8080")
			go func() { defer close(done); s.ServeHTTP(writer, r) }()
			<-started
			for _, other := range []string{"upload", "process", "download"} {
				var got *httptest.ResponseRecorder
				switch other {
				case "upload":
					_, got = upload(t, s, "z\ny\n")
				case "process":
					got = request(s, "/api/process", "application/json", tok, strings.NewReader("{}"))
				case "download":
					got = request(s, "/api/download", "application/octet-stream", tok, strings.NewReader(""))
				}
				if got.Code != 409 {
					t.Errorf("%s competing with %s got %d", other, active, got.Code)
				}
			}
			close(release)
			<-done
			_, w = upload(t, s, "z\ny\n")
			if w.Code != 200 {
				t.Fatalf("gate not released: %d", w.Code)
			}
		})
	}
}
func TestShutdownWhileOperationActive(t *testing.T) {
	m := jobs.New(nil)
	s := New(m, "127.0.0.1:8080")
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	r := originRequest("POST", "http://127.0.0.1:8080/api/upload", &blockedReader{started: started, release: release, body: bytes.NewReader(multipartBody(`form-data; name="file"; filename="x"`, "a\nx\n"))})
	r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	w := httptest.NewRecorder()
	go func() { defer close(done); s.ServeHTTP(w, r) }()
	<-started
	m.Close()
	close(release)
	<-done
	if w.Code == 200 && strings.Contains(w.Body.String(), `"token"`) {
		t.Fatal("upload committed after shutdown")
	}
	if _, err := m.Acquire(true, ""); !errors.Is(err, jobs.ErrClosed) {
		t.Fatal(err)
	}
}

func TestDownloadHeadersAndFilename(t *testing.T) {
	m := jobs.New(nil)
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	body := multipartBody(`form-data; name="file"; filename="evil\r\nInjected.csv"`, "a\nx\n")
	w := rawUpload(s, body)
	var v struct {
		Token string `json:"token"`
	}
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	w = request(s, "/api/process", "application/json", v.Token, strings.NewReader("{}"))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request(s, "/api/download", "application/octet-stream", v.Token, strings.NewReader(""))
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/csv; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal(w.Code, w.Header())
	}
	d := w.Header().Get("Content-Disposition")
	if strings.ContainsAny(d, "\r\n") || !strings.HasPrefix(d, `attachment; filename="`) {
		t.Fatal(d)
	}
}
func TestJSONSizeLimit(t *testing.T) {
	m := jobs.New(nil)
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	tok, w := upload(t, s, "a\nx\n")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request(s, "/api/process", "application/json", tok, strings.NewReader("{}"+strings.Repeat(" ", 4095)))
	if w.Code != 413 {
		t.Fatal(w.Code, w.Body)
	}
}

type overLimitReader struct{}

func (overLimitReader) Read([]byte) (int, error) { return 0, &http.MaxBytesError{Limit: maxBody} }
func TestMultipartLimitDuringParser(t *testing.T) {
	m := jobs.New(nil)
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	input := io.MultiReader(strings.NewReader("--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x\"\r\n\r\nh\n"), overLimitReader{})
	w := request(s, "/api/upload", "multipart/form-data; boundary=b", "", input)
	if w.Code != 413 || !strings.Contains(w.Body.String(), "body_too_large") {
		t.Fatal(w.Code, w.Body)
	}
}
func TestCancelledActiveUploadPreservesJob(t *testing.T) {
	m := jobs.New(nil)
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	tok, w := upload(t, s, "a\nx\n")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	r := originRequest("POST", "http://127.0.0.1:8080/api/upload", &blockedReader{started: started, release: release, body: bytes.NewReader(multipartBody(`form-data; name="file"; filename="x"`, "b\ny\n"))}).WithContext(ctx)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	go func() { defer close(done); s.ServeHTTP(httptest.NewRecorder(), r) }()
	<-started
	cancel()
	close(release)
	<-done
	w = request(s, "/api/process", "application/json", tok, strings.NewReader("{}"))
	if w.Code != 200 {
		t.Fatalf("old job lost or gate held: %d", w.Code)
	}
}

func TestUploadDoesNotUseTemporaryDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("TMP", dir)
	if os.TempDir() != dir {
		t.Fatalf("temporary directory not selected: %s", os.TempDir())
	}
	m := jobs.New(nil)
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	_, w := upload(t, s, "a\nx\n")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = rawUpload(s, multipartBody(`form-data; name="file"; filename="x"`, "a\n"+strings.Repeat("x", 10*1024*1024)))
	if w.Code != 413 {
		t.Fatal(w.Code)
	}
	w = rawUpload(s, []byte("--b\r\ninvalid"))
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := originRequest("POST", "http://127.0.0.1:8080/api/upload", bytes.NewReader(multipartBody(`form-data; name="file"; filename="x"`, "a\nx\n"))).WithContext(ctx)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	s.ServeHTTP(httptest.NewRecorder(), r)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temp files: %v, error %v", entries, err)
	}
}
