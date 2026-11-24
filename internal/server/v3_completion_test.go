package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"csv-cleanroom/internal/csvdata"
	"csv-cleanroom/internal/jobs"
)

type completionState struct {
	job                *jobs.Job
	original, snapshot []byte
}

func completionCapture(t *testing.T, s *Server, tok string) completionState {
	t.Helper()
	l, e := s.Jobs.Acquire(false, tok)
	if e != nil {
		t.Fatal(e)
	}
	defer l.End(false)
	o, e := json.Marshal(l.Job.Original)
	if e != nil {
		t.Fatal(e)
	}
	p, e := json.Marshal(l.Job.Snapshot)
	if e != nil {
		t.Fatal(e)
	}
	return completionState{l.Job, o, p}
}
func completionUnchanged(t *testing.T, s *Server, tok string, b completionState) {
	t.Helper()
	a := completionCapture(t, s, tok)
	if a.job != b.job || string(a.original) != string(b.original) || string(a.snapshot) != string(b.snapshot) {
		t.Fatal("original, snapshot, preview or report changed")
	}
}

type completionWriter struct {
	h        http.Header
	statuses []int
	writes   int
	payload  []byte
}

func (w *completionWriter) Header() http.Header { return w.h }
func (w *completionWriter) WriteHeader(n int)   { w.statuses = append(w.statuses, n) }
func (w *completionWriter) Write(p []byte) (int, error) {
	w.writes++
	w.payload = append(w.payload, p...)
	return 0, errors.New("body write failed")
}

func completionReq(path, tok string, fields []string) *http.Request {
	body, typ := "", "application/octet-stream"
	if path == "/api/process" {
		body, typ = "{}", "application/json"
	}
	r := originRequest("POST", "http://127.0.0.1:8080"+path, strings.NewReader(body))
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Content-Type", typ)
	for _, v := range fields {
		if v == "VALID" {
			v = tok
		}
		r.Header.Add("X-CSV-Cleanroom-Token", v)
	}
	return r
}
func completionFailure(t *testing.T, s *Server, tok, kind, path string) {
	t.Helper()
	switch kind {
	case "writer":
		w := &completionWriter{h: make(http.Header)}
		w.WriteHeader(http.StatusOK)
		s.ServeHTTP(w, v3request(tok))
		if len(w.statuses) != 1 || w.statuses[0] != 200 || w.writes != 1 || !strings.HasPrefix(string(w.payload), "name\n") || strings.Contains(string(w.payload), "\"error\"") {
			t.Fatalf("precommitted writer: %#v", w)
		}
	case "absent", "empty", "duplicate_identical", "duplicate_conflicting":
		var fields []string
		switch kind {
		case "empty":
			fields = []string{""}
		case "duplicate_identical":
			fields = []string{"VALID", "VALID"}
		case "duplicate_conflicting":
			fields = []string{"VALID", "wrong"}
		}
		r := completionReq(path, tok, fields)
		if kind == "empty" {
			v, ok := r.Header[http.CanonicalHeaderKey("X-CSV-Cleanroom-Token")]
			if !ok || len(v) != 1 || v[0] != "" {
				t.Fatal("empty header omitted")
			}
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		var v struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if w.Code != 401 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Error.Code != "invalid_or_expired_job" || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), tok) || strings.Contains(w.Body.String(), "wrong") {
			t.Fatalf("rejection %d %s", w.Code, w.Body)
		}
	case "random":
		saved := rand.Reader
		func() {
			defer func() { rand.Reader = saved }()
			rand.Reader = v3errReader{}
			w := rawUpload(s, multipartBody(`form-data; name="file"; filename="new.csv"`, "new\nvalue\n"))
			if w.Code != 500 || !strings.Contains(w.Body.String(), "internal_error") || strings.Contains(w.Body.String(), tok) || strings.Contains(w.Body.String(), "value") {
				t.Fatal(w.Code, w.Body)
			}
		}()
	case "filename":
		w := rawUpload(s, multipartBody(`form-data; name="file"; filename="`+strings.Repeat("a", 256)+`"`, "new\nvalue\n"))
		if w.Code != 400 || strings.Contains(w.Body.String(), tok) || strings.Contains(w.Body.String(), "value") {
			t.Fatal(w.Code, w.Body)
		}
	case "processing":
		w := request(s, "/api/process", "application/json", tok, strings.NewReader("{"))
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_json") {
			t.Fatal(w.Code, w.Body)
		}
	}
}
func TestV3CompletionFailures(t *testing.T) {
	cases := []struct{ kind, path string }{{"writer", ""}, {"random", ""}, {"filename", ""}, {"processing", ""}}
	for _, path := range []string{"/api/process", "/api/download"} {
		for _, kind := range []string{"absent", "empty", "duplicate_identical", "duplicate_conflicting"} {
			cases = append(cases, struct{ kind, path string }{kind, path})
		}
	}
	for _, tc := range cases {
		t.Run(tc.kind+tc.path, func(t *testing.T) {
			for _, check := range []string{"expiry", "subsequent"} {
				t.Run(check, func(t *testing.T) {
					now := time.Now()
					s, tok := v3setup(t, func() time.Time { return now })
					before := completionCapture(t, s, tok)
					calls := 0
					s.processData = func(d *csvdata.Dataset, o csvdata.Options) (*csvdata.Dataset, *csvdata.Report, error) {
						calls++
						return csvdata.Process(d, o)
					}
					now = now.Add(jobs.Lifetime - time.Second)
					completionFailure(t, s, tok, tc.kind, tc.path)
					if calls != 0 {
						t.Fatal("rejected request entered processing")
					}
					completionUnchanged(t, s, tok, before)
					if check == "expiry" {
						now = now.Add(time.Second)
						if w := v3download(s, tok); w.Code != 401 {
							t.Fatalf("failure refreshed expiry: %d", w.Code)
						}
						return
					}
					w := v3download(s, tok)
					if w.Code != 200 || w.Body.String() != "name\nsecret\n" {
						t.Fatalf("subsequent operation %d %s", w.Code, w.Body)
					}
					if tc.path != "" {
						r := completionReq(tc.path, tok, []string{"VALID"})
						out := httptest.NewRecorder()
						s.ServeHTTP(out, r)
						if out.Code != 200 {
							t.Fatalf("valid single token %d %s", out.Code, out.Body)
						}
						if tc.path == "/api/process" && calls != 1 {
							t.Fatal("valid process did not run")
						}
					}
				})
			}
		})
	}
}

func TestV3CompletionHTTPExpiry(t *testing.T) {
	var nanos atomic.Int64
	start := time.Now()
	nanos.Store(start.UnixNano())
	clock := func() time.Time { return time.Unix(0, nanos.Load()) }
	s, tok := v3setup(t, clock)
	before := completionCapture(t, s, tok)
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	s.Host = listener.Addr().String()
	srv := &http.Server{Handler: s}
	done := make(chan struct{})
	go func() { _ = srv.Serve(listener); close(done) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	entered, release := make(chan struct{}), make(chan struct{})
	s.processData = func(d *csvdata.Dataset, o csvdata.Options) (*csvdata.Dataset, *csvdata.Report, error) {
		return csvdata.ProcessWithCheckpoint(d, o, func() { close(entered); <-release })
	}
	nanos.Store(start.Add(jobs.Lifetime - time.Second).UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, e := http.NewRequestWithContext(ctx, "POST", "http://"+s.Host+"/api/process", strings.NewReader(`{}`))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Origin", "http://"+s.Host)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSV-Cleanroom-Token", tok)
	type result struct {
		status int
		body   []byte
		err    error
	}
	out := make(chan result, 1)
	go func() {
		res, e := http.DefaultClient.Do(r)
		if e != nil {
			out <- result{err: e}
			return
		}
		defer res.Body.Close()
		b, e := io.ReadAll(res.Body)
		out <- result{res.StatusCode, b, e}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("checkpoint not reached")
	}
	if _, e = s.Jobs.Acquire(false, tok); !errors.Is(e, jobs.ErrBusy) {
		t.Fatalf("not admitted: %v", e)
	}
	nanos.Store(start.Add(jobs.Lifetime + time.Second).UnixNano())
	close(release)
	var got result
	select {
	case got = <-out:
	case <-ctx.Done():
		t.Fatal("processing did not finish")
	}
	if got.err != nil || got.status != 200 {
		t.Fatalf("processing %d %s %v", got.status, got.body, got.err)
	}
	after := completionCapture(t, s, tok)
	if after.job != before.job || string(after.original) != string(before.original) || string(after.snapshot) == string(before.snapshot) {
		t.Fatal("unexpected publication")
	}
	l, e := s.Jobs.Acquire(false, tok)
	if e != nil {
		t.Fatal(e)
	}
	if l.Job.Snapshot.Data.Rows[0][0] != " secret " || l.Job.Snapshot.Preview.Rows[0][0] != " secret " || l.Job.Snapshot.Report.CellsTrimmed.Applied {
		t.Fatal("mixed published generation")
	}
	l.End(false)
	download, e := http.NewRequestWithContext(ctx, "POST", "http://"+s.Host+"/api/download", nil)
	if e != nil {
		t.Fatal(e)
	}
	download.Header.Set("Origin", "http://"+s.Host)
	download.Header.Set("Content-Type", "application/octet-stream")
	download.Header.Set("X-CSV-Cleanroom-Token", tok)
	downloadResponse, e := http.DefaultClient.Do(download)
	if e != nil {
		t.Fatal(e)
	}
	downloadBody, e := io.ReadAll(downloadResponse.Body)
	downloadResponse.Body.Close()
	if e != nil || downloadResponse.StatusCode != 200 || string(downloadBody) != "name\n\" secret \"\n" {
		t.Fatalf("subsequent HTTP download: %d %q %v", downloadResponse.StatusCode, downloadBody, e)
	}
	// Advance to just before the refreshed boundary without performing a refreshing operation.
	finished := start.Add(jobs.Lifetime + time.Second)
	nanos.Store(finished.Add(jobs.Lifetime - time.Nanosecond).UnixNano())
	l, e = s.Jobs.Acquire(false, tok)
	if e != nil {
		t.Fatal("refreshed token invalid", e)
	}
	l.End(false)
	nanos.Store(finished.Add(jobs.Lifetime).UnixNano())
	calls := 0
	s.processData = func(d *csvdata.Dataset, o csvdata.Options) (*csvdata.Dataset, *csvdata.Report, error) {
		calls++
		return csvdata.Process(d, o)
	}
	r, e = http.NewRequestWithContext(ctx, "POST", "http://"+s.Host+"/api/process", strings.NewReader("{}"))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Origin", "http://"+s.Host)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSV-Cleanroom-Token", tok)
	res, e := http.DefaultClient.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 401 || calls != 0 || !strings.Contains(string(b), "invalid_or_expired_job") {
		t.Fatalf("boundary %d %s calls %d", res.StatusCode, b, calls)
	}
	if _, w := upload(t, New(s.Jobs, "127.0.0.1:8080"), "a\nb\n"); w.Code != 200 {
		t.Fatal("admission after expiry")
	}
}
