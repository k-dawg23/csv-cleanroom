package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"csv-cleanroom/internal/jobs"
)

func TestEmbeddedInterface(t *testing.T) {
	s := New(jobs.New(nil), "127.0.0.1:8080")
	for _, tc := range []struct{ path, contentType, contains string }{
		{"/", "text/html", "CSV Cleanroom"}, {"/assets/style.css", "text/css", "#101b35"}, {"/assets/app.js", "text/javascript", "X-CSV-Cleanroom-Token"}, {"/assets/one-week-builds-logo.png", "image/png", "\x89PNG\r\n\x1a\n"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r.Host = "127.0.0.1:8080"
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), tc.contentType) || !strings.Contains(w.Body.String(), tc.contains) {
				t.Fatalf("unexpected response: %d %s", w.Code, w.Header().Get("Content-Type"))
			}
		})
	}
	for _, path := range []string{"/assets/", "/assets/unknown", "/web/index.html", "/server.go"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = "127.0.0.1:8080"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Errorf("%s: got %d", path, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/assets/one-week-builds-logo.png", nil)
	r.Host = "localhost:8080"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("bad host: %d", w.Code)
	}
	r = httptest.NewRequest(http.MethodPost, "/", nil)
	r.Host = "127.0.0.1:8080"
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Errorf("wrong method: %d", w.Code)
	}
}
