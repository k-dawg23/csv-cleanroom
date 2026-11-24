package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"csv-cleanroom/internal/jobs"
)

func v3setup(t *testing.T, clock func() time.Time) (*Server, string) {
	t.Helper()
	m := jobs.New(clock)
	t.Cleanup(m.Close)
	s := New(m, "127.0.0.1:8080")
	tok, w := upload(t, s, "name\n secret \n")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = request(s, "/api/process", "application/json", tok, strings.NewReader(`{"trimWhitespace":true}`))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	return s, tok
}
func v3download(s *Server, tok string) *httptest.ResponseRecorder {
	return request(s, "/api/download", "application/octet-stream", tok, strings.NewReader(""))
}
func v3request(tok string) *http.Request {
	r := originRequest("POST", "http://127.0.0.1:8080/api/download", nil)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("X-CSV-Cleanroom-Token", tok)
	return r
}

type v3writer struct {
	h               http.Header
	mode            string
	writes, headers int
	data            []byte
}

func (w *v3writer) Header() http.Header { return w.h }
func (w *v3writer) WriteHeader(int)     { w.headers++ }
func (w *v3writer) Write(p []byte) (int, error) {
	w.writes++
	w.headers++
	switch w.mode {
	case "zero":
		return 0, errors.New("failed")
	case "partial":
		w.data = append(w.data, p[:2]...)
		return 2, errors.New("failed")
	case "short":
		w.data = append(w.data, p[:2]...)
		return 2, nil
	}
	w.data = append(w.data, p...)
	return len(p), nil
}
func (w *v3writer) FlushError() error {
	if w.mode == "flush" {
		return errors.New("failed")
	}
	return nil
}
func TestV3WriterFailures(t *testing.T) {
	for _, mode := range []string{"zero", "partial", "short", "flush"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			s, tok := v3setup(t, func() time.Time { return now })
			l, e := s.Jobs.Acquire(false, tok)
			if e != nil {
				t.Fatal(e)
			}
			job, snap, orig := l.Job, l.Job.Snapshot, l.Job.Original
			l.End(false)
			now = now.Add(jobs.Lifetime - time.Second)
			for i := 0; i < 2; i++ {
				w := &v3writer{h: make(http.Header), mode: mode}
				s.ServeHTTP(w, v3request(tok))
				if w.writes != 1 || w.headers != 1 {
					t.Fatalf("writes/headers %d/%d", w.writes, w.headers)
				}
				l, e = s.Jobs.Acquire(false, tok)
				if e != nil || l.Job != job || l.Job.Snapshot != snap || l.Job.Original != orig || orig.Rows[0][0] != " secret " || snap.Data.Rows[0][0] != "secret" {
					t.Fatal("state or admission", e)
				}
				l.End(false)
			}
			now = now.Add(time.Second)
			if w := v3download(s, tok); w.Code != 401 {
				t.Fatalf("failure refreshed expiry: %d", w.Code)
			}
		})
	}
}
func TestV3DownloadContract(t *testing.T) {
	s, tok := v3setup(t, nil)
	w := v3download(s, tok)
	if w.Code != 200 || w.Body.String() != "name\nsecret\n" || !utf8.Valid(w.Body.Bytes()) || w.Header().Get("Content-Type") != "text/csv; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Length") != fmt.Sprint(w.Body.Len()) {
		t.Fatal(w.Code, w.Header(), w.Body)
	}
	typ, p, e := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
	if e != nil || typ != "attachment" || p["filename"] != "example_cleaned.csv" {
		t.Fatal(typ, p, e)
	}
	for k, v := range w.Header() {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") || strings.Contains(fmt.Sprint(v), tok) || strings.Contains(fmt.Sprint(v), "/home/") {
			t.Fatal(k, v)
		}
	}
	bad := v3download(s, "wrong")
	if bad.Code != 401 || strings.Contains(bad.Body.String(), "secret") || strings.Contains(bad.Body.String(), tok) || !strings.Contains(bad.Body.String(), "invalid_or_expired_job") {
		t.Fatal(bad.Code, bad.Body)
	}
}
func TestV3Filenames(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("TMP", dir)
	if os.TempDir() != dir {
		t.Fatalf("temporary directory not selected: %s", os.TempDir())
	}
	cases := []struct {
		name, disp, want string
		status           int
	}{
		{"slash", `form-data; name="file"; filename="../dir/file.csv"`, "file_cleaned.csv", 200},
		{"windows", `form-data; name="file"; filename="C:\\temp\\file.csv"`, "file_cleaned.csv", 200},
		{"absolute", `form-data; name="file"; filename="/etc/passwd"`, "passwd_cleaned.csv", 200},
		{"quote", `form-data; name="file"; filename="a\"b.csv"`, "a_b_cleaned.csv", 200},
		{"unicode", `form-data; name="file"; filename="café.奇"`, "caf_cleaned.csv", 200},
		{"multibyte255", `form-data; name="file"; filename="` + strings.Repeat("é", 127) + "a\"", "a_cleaned.csv", 200},
		{"multibyte256", `form-data; name="file"; filename="` + strings.Repeat("é", 128) + "\"", "", 400},
		{"ascii255", `form-data; name="file"; filename="` + strings.Repeat("a", 255) + "\"", strings.Repeat("a", 80) + "_cleaned.csv", 200},
		{"ascii256", `form-data; name="file"; filename="` + strings.Repeat("a", 256) + "\"", "", 400},
		{"empty", `form-data; name="file"; filename=""`, "", 400},
		{"extension", `form-data; name="file"; filename=".weird"`, "cleaned.csv", 200},
		{"injection", "form-data; name=\"file\"; filename=\"x\r\nX-Evil: yes.csv\"", "", 400},
		{"control", "form-data; name=\"file\"; filename=\"x\x01.csv\"", "", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, old := v3setup(t, nil)
			w := rawUpload(s, multipartBody(tc.disp, "x\ny\n"))
			if w.Code != tc.status {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if tc.status != 200 {
				if v3download(s, old).Code != 200 {
					t.Fatal("previous job lost")
				}
				return
			}
			var v struct {
				Token string `json:"token"`
			}
			if json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Token == "" {
				t.Fatal(w.Body)
			}
			if request(s, "/api/process", "application/json", v.Token, strings.NewReader("{}")).Code != 200 {
				t.Fatal("process")
			}
			d := v3download(s, v.Token)
			typ, p, e := mime.ParseMediaType(d.Header().Get("Content-Disposition"))
			if d.Code != 200 || e != nil || typ != "attachment" || p["filename"] != tc.want || strings.ContainsAny(d.Header().Get("Content-Disposition"), "\r\n") || d.Header().Get("X-Evil") != "" {
				t.Fatal(d.Code, d.Header(), p, e)
			}
		})
	}
	entries, e := os.ReadDir(dir)
	if e != nil || len(entries) != 0 {
		t.Fatal(entries, e)
	}
}

type v3errReader struct{}

func (v3errReader) Read([]byte) (int, error) { return 0, errors.New("random failure") }
func TestV3TokenGeneration(t *testing.T) {
	s, tok := v3setup(t, nil)
	seen := map[string]bool{tok: true}
	for i := 0; i < 100; i++ {
		next, w := upload(t, s, "h\nv\n")
		b, e := base64.RawURLEncoding.DecodeString(next)
		if w.Code != 200 || len(next) != 43 || e != nil || len(b) != 32 || seen[next] {
			t.Fatal("invalid or duplicate token")
		}
		if request(s, "/api/process", "application/json", tok, strings.NewReader("{}")).Code != 401 {
			t.Fatal("old token valid")
		}
		seen[next] = true
		tok = next
	}
}
func TestV3RandomFailure(t *testing.T) {
	s, tok := v3setup(t, nil)
	l, e := s.Jobs.Acquire(false, tok)
	if e != nil {
		t.Fatal(e)
	}
	old, snap := l.Job, l.Job.Snapshot
	l.End(false)
	saved := rand.Reader
	rand.Reader = v3errReader{}
	w := rawUpload(s, multipartBody(`form-data; name="file"; filename="new.csv"`, "h\nnew\n"))
	rand.Reader = saved
	if w.Code != 500 || !strings.Contains(w.Body.String(), "internal_error") || strings.Contains(w.Body.String(), tok) || strings.Contains(w.Body.String(), "new") {
		t.Fatal(w.Code, w.Body)
	}
	l, e = s.Jobs.Acquire(false, tok)
	if e != nil || l.Job != old || l.Job.Snapshot != snap {
		t.Fatal("state lost", e)
	}
	l.End(false)
	if v3download(s, tok).Code != 200 {
		t.Fatal("download failed")
	}
	_, w = upload(t, s, "h\nnew\n")
	if w.Code != 200 {
		t.Fatal("subsequent upload", w.Code)
	}
}
func TestV3TokenValidation(t *testing.T) {
	s, tok := v3setup(t, nil)
	for _, path := range []string{"/api/process", "/api/download"} {
		for _, v := range []string{"", "bad", strings.Repeat("a", 43), tok[:42] + "!", " " + tok, tok + " ", "é" + tok} {
			typ, body := "application/octet-stream", ""
			if path == "/api/process" {
				typ, body = "application/json", "{}"
			}
			w := request(s, path, typ, v, strings.NewReader(body))
			if w.Code != 401 || !strings.Contains(w.Body.String(), "invalid_or_expired_job") || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), tok) {
				t.Fatalf("%s %q %d %s", path, v, w.Code, w.Body)
			}
		}
	}
	if v3download(s, tok).Code != 200 {
		t.Fatal("valid token lost")
	}
}
func TestV3Expiry(t *testing.T) {
	for _, action := range []string{"none", "process", "download", "failed_process", "failed_download", "unauthorized", "rejected_upload"} {
		t.Run(action, func(t *testing.T) {
			now := time.Now()
			s, tok := v3setup(t, func() time.Time { return now })
			now = now.Add(jobs.Lifetime - time.Second)
			switch action {
			case "process":
				if request(s, "/api/process", "application/json", tok, strings.NewReader("{}")).Code != 200 {
					t.Fatal(action)
				}
			case "download":
				if v3download(s, tok).Code != 200 {
					t.Fatal(action)
				}
			case "failed_process":
				if request(s, "/api/process", "application/json", tok, strings.NewReader("bad")).Code != 400 {
					t.Fatal(action)
				}
			case "failed_download":
				if request(s, "/api/download", "application/octet-stream", tok, strings.NewReader("x")).Code != 400 {
					t.Fatal(action)
				}
			case "unauthorized":
				if v3download(s, "bad").Code != 401 {
					t.Fatal(action)
				}
			case "rejected_upload":
				if rawUpload(s, []byte("bad")).Code != 400 {
					t.Fatal(action)
				}
			}
			now = now.Add(time.Second)
			l, e := s.Jobs.Acquire(false, tok)
			refresh := action == "process" || action == "download"
			if refresh {
				if e != nil {
					t.Fatal(e)
				}
				l.End(false)
				now = now.Add(jobs.Lifetime - time.Second)
				if _, e = s.Jobs.Acquire(false, tok); !errors.Is(e, jobs.ErrInvalid) {
					t.Fatal("new boundary", e)
				}
			} else if !errors.Is(e, jobs.ErrInvalid) {
				t.Fatal("boundary", e)
			}
			if v3download(s, tok).Code != 401 {
				t.Fatal("expired download")
			}
			if request(s, "/api/process", "application/json", tok, strings.NewReader("{}")).Code != 401 {
				t.Fatal("expired process")
			}
			newtok, w := upload(t, s, "h\nv\n")
			if w.Code != 200 || newtok == tok {
				t.Fatal("new upload")
			}
		})
	}
}
func TestV3AdmittedPastExpiry(t *testing.T) {
	now := time.Now()
	s, tok := v3setup(t, func() time.Time { return now })
	l, e := s.Jobs.Acquire(false, tok)
	if e != nil {
		t.Fatal(e)
	}
	before := l.Job.Snapshot
	now = now.Add(jobs.Lifetime + time.Second)
	if !reflect.DeepEqual(before, l.Job.Snapshot) {
		t.Fatal("changed while admitted")
	}
	l.End(true)
	l, e = s.Jobs.Acquire(false, tok)
	if e != nil {
		t.Fatal(e)
	}
	l.End(false)
}

func TestV3DuplicateTokenAndReplacementExpiry(t *testing.T) {
	now := time.Now()
	s, old := v3setup(t, func() time.Time { return now })
	r := v3request(old)
	r.Header.Add("X-CSV-Cleanroom-Token", old)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	now = now.Add(jobs.Lifetime - time.Second)
	next, out := upload(t, s, "h\nv\n")
	if out.Code != 200 || old == next {
		t.Fatal("replacement")
	}
	if v3download(s, old).Code != 401 {
		t.Fatal("old token accepted")
	}
	now = now.Add(time.Second)
	l, e := s.Jobs.Acquire(false, next)
	if e != nil {
		t.Fatal(e)
	}
	l.End(false)
	now = now.Add(jobs.Lifetime)
	if _, e = s.Jobs.Acquire(false, next); !errors.Is(e, jobs.ErrInvalid) {
		t.Fatal("new token survived expiry", e)
	}
}
