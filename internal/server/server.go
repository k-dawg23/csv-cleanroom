// Package server provides the local-only CSV Cleanroom HTTP API.
package server

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"csv-cleanroom/internal/csvdata"
	"csv-cleanroom/internal/jobs"
)

//go:embed web/index.html web/style.css web/app.js web/one-week-builds-logo.png
var assets embed.FS

const maxBody = 11 * 1024 * 1024
const maxJSON = 4096

type Server struct {
	Jobs        *jobs.Manager
	Host        string
	processData func(*csvdata.Dataset, csvdata.Options) (*csvdata.Dataset, *csvdata.Report, error)
}

// HTTPServer applies the production HTTP parser and timeout configuration.
func HTTPServer(m *jobs.Manager, host string, ctx context.Context) *http.Server {
	handler := New(m, host)
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Context cancellation does not unblock socket I/O; deadlines do so only on shutdown.
		stop := context.AfterFunc(ctx, func() {
			c := http.NewResponseController(w)
			_ = c.SetReadDeadline(time.Now())
			_ = c.SetWriteDeadline(time.Now())
		})
		defer stop()
		handler.ServeHTTP(w, r)
	})
	return &http.Server{Handler: wrapped, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 4096, BaseContext: func(net.Listener) context.Context { return ctx }}
}

func New(m *jobs.Manager, host string) *Server {
	return &Server{Jobs: m, Host: host, processData: csvdata.Process}
}

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code}})
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Go replaces the wire Host with the absolute-form authority; rejecting
	// absolute-form targets prevents a conflicting Host from being hidden.
	if r.URL.IsAbs() || r.Host != s.Host || len(r.Header.Values("Host")) != 0 {
		fail(w, 403, "forbidden")
		return
	}
	size := len(r.Method) + len(r.RequestURI) + len(r.Proto) + len(r.Host) + 12
	for k, vs := range r.Header {
		for _, v := range vs {
			size += len(k) + len(v) + 4
		}
	}
	if size > 8192 {
		fail(w, 413, "headers_too_large")
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/assets/style.css" || r.URL.Path == "/assets/app.js" || r.URL.Path == "/assets/one-week-builds-logo.png" {
		serveAsset(w, r)
		return
	}
	if r.URL.Path != "/api/upload" && r.URL.Path != "/api/process" && r.URL.Path != "/api/download" {
		fail(w, 404, "not_found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, 405, "method_not_allowed")
		return
	}
	origins := r.Header.Values("Origin")
	if len(origins) != 1 || origins[0] != "http://"+s.Host {
		fail(w, 403, "forbidden")
		return
	}
	enc := r.Header.Values("Content-Encoding")
	if len(enc) > 1 || len(enc) == 1 && !strings.EqualFold(enc[0], "identity") {
		fail(w, 415, "unsupported_encoding")
		return
	}
	l, err := s.Jobs.Acquire(r.URL.Path == "/api/upload", token(r))
	if err != nil {
		if errors.Is(err, jobs.ErrBusy) {
			fail(w, 409, "busy")
		} else {
			fail(w, 401, "invalid_or_expired_job")
		}
		return
	}
	success := false
	defer func() { l.End(success) }()
	switch r.URL.Path {
	case "/api/upload":
		success = s.upload(w, r, l)
	case "/api/process":
		success = s.process(w, r, l)
	case "/api/download":
		success = s.download(w, r, l)
	}
}

func serveAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	name, contentType := "web/index.html", "text/html; charset=utf-8"
	switch r.URL.Path {
	case "/assets/style.css":
		name, contentType = "web/style.css", "text/css; charset=utf-8"
	case "/assets/app.js":
		name, contentType = "web/app.js", "text/javascript; charset=utf-8"
	case "/assets/one-week-builds-logo.png":
		name, contentType = "web/one-week-builds-logo.png", "image/png"
	}
	b, err := assets.ReadFile(name)
	if err != nil {
		fail(w, http.StatusInternalServerError, "internal_error")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func token(r *http.Request) string {
	v := r.Header.Values("X-CSV-Cleanroom-Token")
	if len(v) != 1 {
		return ""
	}
	return v[0]
}
func media(r *http.Request, expected string) (map[string]string, bool) {
	vals := r.Header.Values("Content-Type")
	if len(vals) != 1 {
		return nil, false
	}
	t, p, e := mime.ParseMediaType(vals[0])
	return p, e == nil && t == expected
}
func cancelled(ctx context.Context) bool { return ctx.Err() != nil }
func safeCSVError(w http.ResponseWriter, err error) {
	var ce *csvdata.Error
	if errors.As(err, &ce) {
		status := 400
		if ce.Code == "file_too_large" {
			status = 413
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(status)
		detail := map[string]any{"code": ce.Code}
		if ce.Record > 0 {
			detail["record"] = ce.Record
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"error": detail})
		return
	}
	var he *csvdata.HeaderError
	if errors.As(err, &he) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": he.Code, "columns": he.Columns}})
		return
	}
	fail(w, 400, "invalid_request")
}

// limitTracker preserves MaxBytesReader failures even if an intermediate parser
// deliberately replaces underlying read errors with a safe diagnostic.
type limitTracker struct {
	r        io.Reader
	exceeded bool
}

func (t *limitTracker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		t.exceeded = true
	}
	return n, err
}
func uploadError(w http.ResponseWriter, limit *limitTracker, err error) {
	if limit.exceeded {
		fail(w, http.StatusRequestEntityTooLarge, "body_too_large")
		return
	}
	if err != nil {
		safeCSVError(w, err)
		return
	}
	fail(w, http.StatusBadRequest, "invalid_multipart")
}

// upload uses multipart.Reader and never creates temporary files. The bounded
// prefix retains the exact first part header so the standard reader can parse it.
func (s *Server) upload(w http.ResponseWriter, r *http.Request, l *jobs.Lease) bool {
	p, ok := media(r, "multipart/form-data")
	if !ok || p["boundary"] == "" {
		fail(w, 415, "unsupported_media_type")
		return false
	}
	body := &limitTracker{r: http.MaxBytesReader(w, r.Body, maxBody)}
	br := bufio.NewReader(body)
	var prefix bytes.Buffer
	line, err := br.ReadString('\n')
	if err != nil || line != "--"+p["boundary"]+"\r\n" {
		uploadError(w, body, nil)
		return false
	}
	prefix.WriteString(line)
	n := 0
	for {
		line, err = br.ReadString('\n')
		n += len(line)
		if n > 4096 {
			fail(w, 413, "part_headers_too_large")
			return false
		}
		if err != nil {
			uploadError(w, body, nil)
			return false
		}
		prefix.WriteString(line)
		if line == "\r\n" {
			break
		}
	}
	mr := multipart.NewReader(io.MultiReader(&prefix, br), p["boundary"])
	part, err := mr.NextRawPart()
	if err != nil {
		uploadError(w, body, nil)
		return false
	}
	vals := part.Header.Values("Content-Disposition")
	if len(vals) != 1 || len(part.Header.Values("Content-Type")) > 1 || len(part.Header) > 2 || (len(part.Header) == 2 && len(part.Header.Values("Content-Type")) != 1) {
		fail(w, 400, "invalid_multipart")
		return false
	}
	disposition, params, err := mime.ParseMediaType(vals[0])
	if !uniqueParameters(vals[0]) {
		fail(w, 400, "invalid_multipart")
		return false
	}
	filename, has := params["filename"]
	if err != nil || disposition != "form-data" || params["name"] != "file" || !has || len(params) != 2 || filename == "" || len(filename) > 255 || !utf8.ValidString(filename) {
		fail(w, 400, "invalid_multipart")
		return false
	}
	data, err := csvdata.Parse(part)
	if err != nil {
		uploadError(w, body, err)
		return false
	}
	next, err := mr.NextRawPart()
	if err != io.EOF || next != nil {
		uploadError(w, body, nil)
		return false
	}
	_, err = io.Copy(io.Discard, br)
	if err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			fail(w, 413, "body_too_large")
		} else {
			fail(w, 400, "invalid_multipart")
		}
		return false
	}
	if cancelled(r.Context()) {
		return false
	}
	tok, err := l.CommitUpload(data, filename)
	if err != nil {
		fail(w, 500, "internal_error")
		return false
	}
	respond(w, map[string]any{"token": tok, "preview": preview(data.Preview())})
	return true
}

// uniqueParameters rejects repeated disposition parameters even when their values match.
func uniqueParameters(v string) bool {
	seen := map[string]bool{}
	quoted, escaped := false, false
	start := 0
	segments := []string{}
	for i, c := range v {
		if escaped {
			escaped = false
			continue
		}
		if quoted && c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			quoted = !quoted
		}
		if c == ';' && !quoted {
			segments = append(segments, v[start:i])
			start = i + 1
		}
	}
	if quoted || escaped {
		return false
	}
	segments = append(segments, v[start:])
	for _, part := range segments[1:] {
		key, _, ok := strings.Cut(part, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if !ok || key == "" || seen[key] || strings.Contains(key, "*") {
			return false
		}
		seen[key] = true
	}
	return true
}
func preview(p csvdata.Preview) any {
	return map[string]any{"header": p.Header, "rows": p.Rows, "rowCount": p.RowCount, "columnCount": p.ColumnCount}
}

var errJSONTooLarge = errors.New("body too large")

func readJSON(r *http.Request) (csvdata.Options, error) {
	var o csvdata.Options
	b, e := io.ReadAll(io.LimitReader(r.Body, maxJSON+1))
	if e != nil {
		return o, fmt.Errorf("invalid")
	}
	if len(b) > maxJSON {
		return o, errJSONTooLarge
	}
	d := json.NewDecoder(bytes.NewReader(b))
	t, e := d.Token()
	if e != nil || t != json.Delim('{') {
		return o, fmt.Errorf("invalid")
	}
	seen := map[string]bool{}
	for d.More() {
		t, e = d.Token()
		if e != nil {
			return o, e
		}
		k, ok := t.(string)
		if !ok || seen[k] {
			return o, fmt.Errorf("invalid")
		}
		seen[k] = true
		var raw json.RawMessage
		if e = d.Decode(&raw); e != nil {
			return o, e
		}
		if string(raw) != "true" && string(raw) != "false" {
			return o, fmt.Errorf("invalid")
		}
		v := string(raw) == "true"
		switch k {
		case "normalizeHeaders":
			o.NormalizeHeaders = v
		case "trimWhitespace":
			o.TrimWhitespace = v
		case "removeDuplicates":
			o.RemoveDuplicates = v
		case "disableMissingAnalysis":
			o.DisableMissingAnalysis = v
		default:
			return o, fmt.Errorf("invalid")
		}
	}
	if _, e = d.Token(); e != nil {
		return o, e
	}
	if _, e = d.Token(); e != io.EOF {
		return o, fmt.Errorf("invalid")
	}
	return o, nil
}
func (s *Server) process(w http.ResponseWriter, r *http.Request, l *jobs.Lease) bool {
	if _, ok := media(r, "application/json"); !ok {
		fail(w, 415, "unsupported_media_type")
		return false
	}
	o, err := readJSON(r)
	if err != nil {
		if errors.Is(err, errJSONTooLarge) {
			fail(w, 413, "body_too_large")
			return false
		}
		fail(w, 400, "invalid_json")
		return false
	}
	d, report, err := s.processData(l.Job.Original, o)
	if err != nil {
		safeCSVError(w, err)
		return false
	}
	if cancelled(r.Context()) {
		return false
	}
	snap := &jobs.Snapshot{Data: d, Report: report, Preview: d.Preview()}
	if l.Publish(snap) != nil {
		return false
	}
	respond(w, map[string]any{"preview": preview(snap.Preview), "report": reportJSON(report)})
	return true
}
func reportJSON(r *csvdata.Report) any {
	cols := make([]any, 0, len(r.Missing.Columns))
	for _, c := range r.Missing.Columns {
		cols = append(cols, map[string]any{"position": c.Position, "originalHeading": c.OriginalHeading, "count": c.Count})
	}
	count := func(c csvdata.CountResult) any { return map[string]any{"applied": c.Applied, "count": c.Count} }
	return map[string]any{"originalRows": r.OriginalRows, "originalColumns": r.OriginalColumns, "processedRows": r.ProcessedRows, "processedColumns": r.ProcessedColumns, "headersChanged": count(r.HeadersChanged), "cellsTrimmed": count(r.CellsTrimmed), "duplicateRowsRemoved": count(r.DuplicateRowsRemoved), "missing": map[string]any{"applied": r.Missing.Applied, "columns": cols, "totalCells": r.Missing.TotalCells, "rowsWithMissing": r.Missing.RowsWithMissing}}
}
func (s *Server) download(w http.ResponseWriter, r *http.Request, l *jobs.Lease) bool {
	if _, ok := media(r, "application/octet-stream"); !ok {
		fail(w, 415, "unsupported_media_type")
		return false
	}
	var one [1]byte
	n, e := r.Body.Read(one[:])
	if n != 0 || e != io.EOF {
		fail(w, 400, "invalid_body")
		return false
	}
	if l.Job.Snapshot == nil {
		fail(w, 400, "not_processed")
		return false
	}
	data, e := csvdata.Export(l.Job.Snapshot.Data)
	if e != nil {
		fail(w, 500, "internal_error")
		return false
	}
	if cancelled(r.Context()) {
		return false
	}
	name := csvdata.CleanedFilename(l.Job.Filename)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	if !strings.Contains(disposition, `filename="`) {
		disposition = "attachment; filename=" + strconv.Quote(name)
	}
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	n, e = w.Write(data)
	if e != nil || n != len(data) || cancelled(r.Context()) {
		return false
	}
	if f, ok := w.(interface{ FlushError() error }); ok && f.FlushError() != nil {
		return false
	}
	return !cancelled(r.Context())
}
