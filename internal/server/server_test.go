package server

import (
	"bytes"
	"csv-cleanroom/internal/jobs"
	"encoding/csv"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// originRequest creates the origin-form requests normally received by the server.
func originRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.URL.Scheme = ""
	r.URL.Host = ""
	r.RequestURI = r.URL.RequestURI()
	return r
}
func request(s *Server, path, typ, token string, body io.Reader) *httptest.ResponseRecorder {
	r := originRequest("POST", "http://127.0.0.1:8080"+path, body)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Content-Type", typ)
	if token != "" {
		r.Header.Set("X-CSV-Cleanroom-Token", token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func upload(t *testing.T, s *Server, data string) (string, *httptest.ResponseRecorder) {
	t.Helper()
	var b bytes.Buffer
	m := multipart.NewWriter(&b)
	p, e := m.CreateFormFile("file", "example.csv")
	if e != nil {
		t.Fatal(e)
	}
	_, _ = io.WriteString(p, data)
	_ = m.Close()
	w := request(s, "/api/upload", m.FormDataContentType(), "", &b)
	var v struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v.Token, w
}
func TestHTTPWorkflow(t *testing.T) {
	for _, tc := range []struct{ input, options string }{{"Name\n\"\"\nAlice\n", "{}"}, {"Name\n\"   \"\nAlice\n", `{"trimWhitespace":true}`}} {
		m := jobs.New(nil)
		s := New(m, "127.0.0.1:8080")
		tok, w := upload(t, s, tc.input)
		if w.Code != 200 || tok == "" {
			t.Fatalf("upload: %d %s", w.Code, w.Body)
		}
		w = request(s, "/api/process", "application/json", tok, strings.NewReader(tc.options))
		if w.Code != 200 {
			t.Fatalf("process: %d %s", w.Code, w.Body)
		}
		w = request(s, "/api/download", "application/octet-stream", tok, strings.NewReader(""))
		if w.Code != 200 || w.Header().Get("Content-Type") != "text/csv; charset=utf-8" {
			t.Fatalf("download: %d", w.Code)
		}
		records, e := csv.NewReader(w.Body).ReadAll()
		if e != nil || len(records) != 3 || records[1][0] != "" || records[2][0] != "Alice" {
			t.Fatalf("records: %#v %v", records, e)
		}
		m.Close()
	}
}
func TestSecurityAndReplacement(t *testing.T) {
	m := jobs.New(nil)
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	for _, host := range []string{"localhost:8080", "127.0.0.1:8080.evil", "127.0.0.1"} {
		r := originRequest("POST", "http://127.0.0.1:8080/api/upload", nil)
		r.Host = host
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("host %s: %d", host, w.Code)
		}
	}
	for _, origin := range []string{"", "null", "http://127.0.0.1:8080.evil", "https://127.0.0.1:8080"} {
		r := originRequest("POST", "http://127.0.0.1:8080/api/upload", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("origin %q: %d", origin, w.Code)
		}
	}
	a, w := upload(t, s, "a\nx\n")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	b, w := upload(t, s, "b\ny\n")
	if w.Code != 200 || a == b {
		t.Fatal("replacement")
	}
	w = request(s, "/api/process", "application/json", a, strings.NewReader("{}"))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	w = request(s, "/api/process", "application/json", b, strings.NewReader(`{"trimWhitespace":true,"trimWhitespace":false}`))
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}
func TestAdmissionAndExpiry(t *testing.T) {
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
	w = request(s, "/api/download", "application/octet-stream", tok, strings.NewReader(""))
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	now = now.Add(jobs.Lifetime)
	l.End(false)
	w = request(s, "/api/process", "application/json", tok, strings.NewReader("{}"))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}
func TestMethodsAndMalformed(t *testing.T) {
	m := jobs.New(nil)
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	r := originRequest(http.MethodGet, "http://127.0.0.1:8080/api/upload", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
	_, w = upload(t, s, "secret,\n\"bad\n")
	if w.Code != 400 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestReprocessingPreservesOriginalAndFailure(t *testing.T) {
	m := jobs.New(nil)
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	tok, w := upload(t, s, "A,A\n x , x \n")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request(s, "/api/process", "application/json", tok, strings.NewReader(`{"trimWhitespace":true}`))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request(s, "/api/process", "application/json", tok, strings.NewReader(`{"normalizeHeaders":true}`))
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	w = request(s, "/api/download", "application/octet-stream", tok, strings.NewReader(""))
	rec, e := csv.NewReader(w.Body).ReadAll()
	if e != nil || rec[1][0] != "x" {
		t.Fatalf("failed process changed snapshot: %v %v", rec, e)
	}
	w = request(s, "/api/process", "application/json", tok, strings.NewReader(`{}`))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request(s, "/api/download", "application/octet-stream", tok, strings.NewReader(""))
	rec, e = csv.NewReader(w.Body).ReadAll()
	if e != nil || rec[1][0] != " x " {
		t.Fatalf("original changed: %v %v", rec, e)
	}
}
