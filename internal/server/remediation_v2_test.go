package server

import (
	"context"
	"csv-cleanroom/internal/csvdata"
	"csv-cleanroom/internal/jobs"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestV2RemediationCandidateCancellation(t *testing.T) {
	m := jobs.New(nil)
	defer m.Close()
	s := New(m, "127.0.0.1:8080")
	tok, w := upload(t, s, " A \n value \n")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request(s, "/api/process", "application/json", tok, strings.NewReader("{}"))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	prior := v2Published(t, m, tok)
	priorBytes, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	l, e := m.Acquire(false, tok)
	if e != nil {
		t.Fatal(e)
	}
	original := l.Job.Original
	originalCopy := &csvdata.Dataset{Header: append([]string(nil), original.Header...), Rows: [][]string{append([]string(nil), original.Rows[0]...)}}
	l.End(false)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.processData = func(d *csvdata.Dataset, o csvdata.Options) (*csvdata.Dataset, *csvdata.Report, error) {
		return csvdata.ProcessWithCheckpoint(d, o, func() { close(entered); <-release })
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := originRequest("POST", "http://127.0.0.1:8080/api/process", strings.NewReader(`{"trimWhitespace":true}`)).WithContext(ctx)
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSV-Cleanroom-Token", tok)
	go func() { defer close(done); s.ServeHTTP(httptest.NewRecorder(), r) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("candidate processor not entered")
	}
	if _, err := m.Acquire(false, tok); err != jobs.ErrBusy {
		t.Fatalf("admission: %v", err)
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled processor did not finish")
	}
	afterBytes, err := json.Marshal(v2Published(t, m, tok))
	if err != nil {
		t.Fatal(err)
	}
	if v2Published(t, m, tok) != prior || string(priorBytes) != string(afterBytes) || !reflect.DeepEqual(original, originalCopy) {
		t.Fatal("prior snapshot, report or original changed")
	}
	s.processData = csvdata.Process
	w = request(s, "/api/process", "application/json", tok, strings.NewReader(`{"trimWhitespace":true}`))
	if w.Code != 200 {
		t.Fatalf("subsequent process: %d", w.Code)
	}
	t.Log("cancelled within the transformation row loop; computation completed after release and candidate was discarded")
}
func TestV2RemediationShutdownReleasesLease(t *testing.T) {
	m := jobs.New(nil)
	defer m.Close()
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := HTTPServer(m, l.Addr().String(), ctx)
	done := make(chan struct{})
	go func() { _ = srv.Serve(l); close(done) }()
	defer func() { _ = srv.Close(); <-done }()
	s := &rawServer{t: t, addr: l.Addr().String(), manager: m}
	tok := s.fixture()
	c := s.dial()
	c.send(t, "POST /api/upload HTTP/1.1\r\nHost: "+s.addr+"\r\nOrigin: http://"+s.addr+"\r\nContent-Type: multipart/form-data; boundary=b\r\nContent-Length: 1000\r\n\r\n--b\r\n")
	v2WaitBusy(t, s, tok)
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if e = srv.Shutdown(shutdown); e != nil {
		t.Fatal(e)
	}
	lease, e := m.Acquire(false, tok)
	if e != nil {
		t.Fatalf("lease not released or token lost: %v", e)
	}
	lease.End(false)
	t.Log("shutdown cancellation interrupted read and released admission; prior token valid")
}
func TestV2RemediationRepeatedDisconnectCleanup(t *testing.T) {
	v2ObserveTemp(t)
	s := startRaw(t)
	tok := s.fixture()
	v2WaitNotBusy(t, s, tok)
	runtime.GC()
	before := runtime.NumGoroutine()
	for i := 0; i < 30; i++ {
		c := s.dial()
		c.send(t, "POST /api/upload HTTP/1.1\r\nHost: "+s.addr+"\r\nOrigin: http://"+s.addr+"\r\nContent-Type: multipart/form-data; boundary=b\r\nContent-Length: 1000\r\nExpect: 100-continue\r\n\r\n")
		response, err := http.ReadResponse(c.r, &http.Request{Method: "POST"})
		if err != nil || response.StatusCode != 100 {
			t.Fatalf("upload admission handshake: %v %v", response, err)
		}
		c.send(t, "--b\r\n")
		_ = c.Close()
		v2WaitNotBusy(t, s, tok)
	}
	deadline := time.Now().Add(3 * time.Second)
	var after int
	for {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= before+8 || time.Now().After(deadline) {
			break
		}
		runtime.Gosched()
	}
	t.Logf("goroutines before=%d after=%d (allowance 8)", before, after)
	if after > before+8 {
		t.Fatal("persistent goroutine growth")
	}
	_ = io.Discard
}
