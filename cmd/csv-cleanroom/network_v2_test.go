package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const v2Addr = "127.0.0.1:8080"

func v2Build(t *testing.T) string {
	t.Helper()
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	goTool := filepath.Join(runtime.GOROOT(), "bin", name)
	version := exec.Command(goTool, "version")
	version.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	out, err := version.CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "go version go1.27.4 ") {
		t.Fatalf("expected Go 1.27.4 at %s: %v: %s", goTool, err, out)
	}
	bin := filepath.Join(t.TempDir(), "csv-cleanroom")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command(goTool, "build", "-buildvcs=false", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, out)
	}
	return bin
}
func v2Port(t *testing.T) net.Listener {
	t.Helper()
	l, e := net.Listen("tcp4", v2Addr)
	if e != nil {
		t.Fatalf("required port 8080 unavailable; existing listener was not disturbed: %v", e)
	}
	return l
}

type v2Process struct {
	cmd    *exec.Cmd
	done   chan error
	stderr bytes.Buffer
	stdout chan string
}

func v2Start(t *testing.T, bin string) *v2Process {
	t.Helper()
	p := &v2Process{cmd: exec.Command(bin), done: make(chan error, 1), stdout: make(chan string, 1)}
	tmp := t.TempDir()
	p.cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "TEMP="+tmp, "TMP="+tmp)
	p.cmd.Stderr = &p.stderr
	out, e := p.cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = p.cmd.Start(); e != nil {
		t.Fatal(e)
	}
	go func() { p.done <- p.cmd.Wait() }()
	go func() { line, _ := bufio.NewReader(out).ReadString('\n'); p.stdout <- line }()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(15 * time.Second):
			t.Error("process did not terminate")
		}
		entries, e := os.ReadDir(tmp)
		if e != nil || len(entries) != 0 {
			t.Errorf("observed TMPDIR: %v %v", entries, e)
		}
	})
	return p
}
func v2Ready(t *testing.T, p *v2Process) {
	t.Helper()
	select {
	case line := <-p.stdout:
		if line != "CSV Cleanroom: http://"+v2Addr+"\n" {
			t.Fatalf("startup line %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("startup timed out")
	}
}
func v2Conn(t *testing.T) net.Conn {
	t.Helper()
	c, e := net.DialTimeout("tcp4", v2Addr, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func v2Request(t *testing.T, wire string) int {
	t.Helper()
	c := v2Conn(t)
	defer c.Close()
	if _, e := io.WriteString(c, wire); e != nil {
		t.Fatal(e)
	}
	r, e := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "POST"})
	if e != nil {
		t.Fatal(e)
	}
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	return r.StatusCode
}
func v2Stopped(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, e := net.DialTimeout("tcp4", v2Addr, 100*time.Millisecond)
		if e != nil {
			return
		}
		_ = c.Close()
	}
	t.Fatal("listener still accepts")
}
func v2Wait(t *testing.T, p *v2Process) error {
	t.Helper()
	select {
	case e := <-p.done:
		p.done <- e
		return e
	case <-time.After(14 * time.Second):
		t.Fatal("shutdown exceeded bound")
		return nil
	}
}
func TestV2OccupiedPort(t *testing.T) {
	bin := v2Build(t)
	l := v2Port(t)
	defer l.Close()
	p := v2Start(t, bin)
	e := v2Wait(t, p)
	if e == nil || !strings.Contains(p.stderr.String(), "127.0.0.1:8080") {
		t.Fatalf("startup should fail clearly: %v %s", e, p.stderr.String())
	}
	c, err := net.DialTimeout("tcp4", v2Addr, time.Second)
	if err != nil {
		t.Fatalf("controlled listener lost: %v", err)
	}
	_ = c.Close()
	t.Logf("occupied port rejected; process exit=%v; listener retained", e)
}

func TestV2RemediationShutdownFallback(t *testing.T) {
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	finished := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; close(finished) })}
	go func() { _ = srv.Serve(l); close(done) }()
	c, e := net.DialTimeout("tcp4", l.Addr().String(), time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: local\r\n\r\n")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler not active")
	}
	start := time.Now()
	e = shutdownServer(srv, 100*time.Millisecond)
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("expected deadline: %v", e)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("fallback not bounded")
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, e = c.Read(make([]byte, 1)); e == nil {
		t.Fatal("connection remained open")
	}
	close(release)
	<-finished
	<-done
	t.Log("uncooperative handler caused deadline and forced connection close")
}
