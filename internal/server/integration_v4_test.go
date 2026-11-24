package server

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Exercise replacement and download through the production HTTP server over TCP.
func TestV4HTTPLifecycle(t *testing.T) {
	s := startRaw(t)
	call := func(wire string, want int) (string, http.Header) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			c := s.dial()
			c.send(t, wire)
			status, body, h, _ := c.response(t)
			_ = c.Close()
			if status == 409 && time.Now().Before(deadline) {
				continue
			}
			if status != want {
				t.Fatalf("status %d want %d: %s", status, want, body)
			}
			return body, h
		}
	}
	upload := func(csv string) (string, string) {
		body := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x.csv\"\r\n\r\n" + csv + "\r\n--b--\r\n"
		response, _ := call(s.upload(body), 200)
		var v struct {
			Token   string `json:"token"`
			Preview struct {
				Header []string   `json:"header"`
				Rows   [][]string `json:"rows"`
			} `json:"preview"`
		}
		if json.Unmarshal([]byte(response), &v) != nil || v.Token == "" || len(v.Preview.Rows) != 1 {
			t.Fatalf("upload: %s", response)
		}
		return v.Token, v.Preview.Rows[0][0]
	}
	process := func(token, expected string) {
		body, _ := call(s.req("/api/process", "Content-Type: application/json\r\nX-CSV-Cleanroom-Token: "+token+"\r\n", `{"trimWhitespace":true}`), 200)
		var v struct {
			Preview struct {
				Rows [][]string `json:"rows"`
			} `json:"preview"`
			Report struct {
				ProcessedRows int `json:"processedRows"`
				CellsTrimmed  struct {
					Applied bool `json:"applied"`
					Count   int  `json:"count"`
				} `json:"cellsTrimmed"`
			} `json:"report"`
		}
		if json.Unmarshal([]byte(body), &v) != nil || len(v.Preview.Rows) != 1 || v.Preview.Rows[0][0] != expected || v.Report.ProcessedRows != 1 || !v.Report.CellsTrimmed.Applied || v.Report.CellsTrimmed.Count != 1 {
			t.Fatalf("process: %s", body)
		}
	}
	download := func(token, expected string) {
		body, h := call(v2Download(s, token), 200)
		if body != expected || h.Get("Content-Type") != "text/csv; charset=utf-8" || h.Get("Content-Length") != fmt.Sprint(len(expected)) || h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("download %q headers %v", body, h)
		}
		kind, p, err := mime.ParseMediaType(h.Get("Content-Disposition"))
		if err != nil || kind != "attachment" || p["filename"] != "x_cleaned.csv" {
			t.Fatalf("disposition: %v %v", h, err)
		}
	}
	first, value := upload("col\n old \n")
	if value != " old " {
		t.Fatal(value)
	}
	process(first, "old")
	download(first, "col\nold\n")
	bad := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x.csv\"\r\n\r\ncol\n\"bad\n\r\n--b--\r\n"
	errBody, _ := call(s.upload(bad), 400)
	if strings.Contains(errBody, first) || strings.Contains(errBody, "bad") {
		t.Fatalf("unsafe error: %s", errBody)
	}
	download(first, "col\nold\n")
	second, value := upload("col\n new \n")
	if second == first || value != " new " {
		t.Fatal("replacement preview or token")
	}
	call(s.process(first), 401)
	process(second, "new")
	download(second, "col\nnew\n")
}
