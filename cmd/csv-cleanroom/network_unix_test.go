//go:build !windows

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestV2UnixApplicationProcess(t *testing.T) {
	bin := v2Build(t)
	t.Run("idle_shutdown", func(t *testing.T) {
		l := v2Port(t)
		_ = l.Close()
		p := v2Start(t, bin)
		v2Ready(t, p)
		n := v2Request(t, "GET /missing HTTP/1.1\r\nHost: "+v2Addr+"\r\n\r\n")
		if n != 404 {
			t.Fatal(n)
		}
		if e := p.cmd.Process.Signal(os.Interrupt); e != nil {
			t.Fatal(e)
		}
		if e := v2Wait(t, p); e != nil {
			t.Fatalf("exit: %v stderr=%s", e, p.stderr.String())
		}
		v2Stopped(t)
		l = v2Port(t)
		_ = l.Close()
		t.Log("idle SIGINT exit 0, listener released and connection refused")
	})
	for _, sig := range []struct {
		name   string
		signal os.Signal
	}{{"sigint", os.Interrupt}, {"sigterm", syscall.SIGTERM}} {
		t.Run("active_upload_shutdown_"+sig.name, func(t *testing.T) {
			l := v2Port(t)
			_ = l.Close()
			p := v2Start(t, bin)
			v2Ready(t, p)
			c := v2Conn(t)
			// Wait until the server admits this request and begins reading its body.
			// Otherwise a probe can acquire the lease first and reject the upload.
			_, e := fmt.Fprintf(c, "POST /api/upload HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\nContent-Type: multipart/form-data; boundary=b\r\nContent-Length: 1000\r\nExpect: 100-continue\r\n\r\n", v2Addr, v2Addr)
			if e != nil {
				t.Fatal(e)
			}
			reader := bufio.NewReader(c)
			response, e := http.ReadResponse(reader, &http.Request{Method: "POST"})
			if e != nil || response.StatusCode != http.StatusContinue {
				t.Fatalf("upload admission: response=%v error=%v", response, e)
			}
			_, e = io.WriteString(c, "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x.csv\"\r\n\r\ncol\nx")
			if e != nil {
				t.Fatal(e)
			}
			probe := fmt.Sprintf("POST /api/upload HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\nContent-Type: multipart/form-data; boundary=b\r\nContent-Length: 0\r\n\r\n", v2Addr, v2Addr)
			deadline := time.Now().Add(3 * time.Second)
			for {
				n := v2Request(t, probe)
				if n == 409 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("active request did not acquire lease, status %d", n)
				}
			}
			start := time.Now()
			if e = p.cmd.Process.Signal(sig.signal); e != nil {
				t.Fatal(e)
			}
			v2Stopped(t)
			e = v2Wait(t, p)
			elapsed := time.Since(start)
			if elapsed > 8*time.Second {
				t.Fatalf("unbounded shutdown: %v", elapsed)
			}
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			_, readErr := reader.Read(make([]byte, 1))
			if readErr == nil {
				t.Fatal("active connection did not terminate")
			}
			l = v2Port(t)
			_ = l.Close()
			t.Logf("active blocked upload ended; shutdown elapsed=%v exit=%v stderr=%q read=%v", elapsed, e, p.stderr.String(), readErr)
			if e != nil {
				t.Fatalf("unexpected exit: %v %s", e, p.stderr.String())
			}
		})
	}
}

func TestV2RemediationActiveDownload(t *testing.T) {
	bin := v2Build(t)
	l := v2Port(t)
	_ = l.Close()
	p := v2Start(t, bin)
	v2Ready(t, p)
	csv := "col\n" + strings.Repeat("x", 8*1024*1024) + "\n"
	body := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x.csv\"\r\n\r\n" + csv + "\r\n--b--\r\n"
	c := v2Conn(t)
	_, err := fmt.Fprintf(c, "POST /api/upload HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\nContent-Type: multipart/form-data; boundary=b\r\nContent-Length: %d\r\n\r\n%s", v2Addr, v2Addr, len(body), body)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err = json.NewDecoder(r.Body).Decode(&payload); err != nil || r.StatusCode != 200 || payload.Token == "" {
		t.Fatalf("upload: %d %v", r.StatusCode, err)
	}
	_ = r.Body.Close()
	_ = c.Close()
	proc := fmt.Sprintf("POST /api/process HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\nContent-Type: application/json\r\nX-CSV-Cleanroom-Token: %s\r\nContent-Length: 2\r\n\r\n{}", v2Addr, v2Addr, payload.Token)
	if n := v2Request(t, proc); n != 200 {
		t.Fatalf("process %d", n)
	}
	c = v2Conn(t)
	if err = c.(*net.TCPConn).SetReadBuffer(4096); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(c, "POST /api/download HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\nContent-Type: application/octet-stream\r\nX-CSV-Cleanroom-Token: %s\r\nContent-Length: 0\r\n\r\n", v2Addr, v2Addr, payload.Token)
	if err != nil {
		t.Fatal(err)
	}
	r, err = http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "POST"})
	if err != nil || r.StatusCode != 200 || r.ContentLength < 8*1024*1024 {
		t.Fatalf("download %v %v", r, err)
	}
	var one [1]byte
	if _, err = io.ReadFull(r.Body, one[:]); err != nil {
		t.Fatal(err)
	}
	// The busy response proves the download still holds admission after headers.
	probe := fmt.Sprintf("POST /api/upload HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\nContent-Type: multipart/form-data; boundary=b\r\nContent-Length: 0\r\n\r\n", v2Addr, v2Addr)
	if n := v2Request(t, probe); n != 409 {
		t.Fatalf("download already complete or not admitted: %d", n)
	}
	start := time.Now()
	if err = p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	v2Stopped(t)
	if err = v2Wait(t, p); err != nil {
		t.Fatalf("shutdown: %v %s", err, p.stderr.String())
	}
	if time.Since(start) > 8*time.Second {
		t.Fatal("download shutdown took too long")
	}
	_ = c.Close()
	l = v2Port(t)
	_ = l.Close()
	if strings.Contains(p.stderr.String(), "panic:") {
		t.Fatal(p.stderr.String())
	}
	t.Logf("active download, 409 gate, SIGINT exit 0 in %v", time.Since(start))
}
