package csvdata

import (
	"bytes"
	"encoding/csv"
	"io"
	"strings"
	"unicode/utf8"
)

// Export validates and serializes a processed dataset into a complete in-memory
// response. No output-size limit is imposed; callers must account for the buffer.
func Export(d *Dataset) ([]byte, error) {
	var out bytes.Buffer
	if err := WriteCSV(&out, d); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// WriteCSV validates all records before writing, then streams CSV to dst. A write
// failure can leave partial output. Errors never expose dataset or writer contents.
func WriteCSV(dst io.Writer, d *Dataset) error {
	if err := validateExport(d); err != nil {
		return err
	}
	if dst == nil {
		return &Error{Code: "write_error"}
	}
	w := csv.NewWriter(dst)
	// An initial data BOM must not be mistaken for a file BOM by Parse. Quote
	// the first field only if the standard writer otherwise emits it unquoted.
	var header bytes.Buffer
	hw := csv.NewWriter(&header)
	if err := hw.Write(d.Header); err != nil {
		return &Error{Code: "write_error"}
	}
	hw.Flush()
	if hw.Error() != nil {
		return &Error{Code: "write_error"}
	}
	h := header.Bytes()
	if bytes.HasPrefix(h, []byte("\xef\xbb\xbf")) {
		end := bytes.IndexAny(h, ",\n")
		if err := writeExportBytes(dst, []byte("\"")); err != nil {
			return &Error{Code: "write_error"}
		}
		if err := writeExportBytes(dst, h[:end]); err != nil {
			return &Error{Code: "write_error"}
		}
		if err := writeExportBytes(dst, []byte("\"")); err != nil {
			return &Error{Code: "write_error"}
		}
		h = h[end:]
	}
	if err := writeExportBytes(dst, h); err != nil {
		return &Error{Code: "write_error"}
	}
	for _, row := range d.Rows {
		if len(row) == 1 && row[0] == "" {
			w.Flush()
			if w.Error() != nil {
				return &Error{Code: "write_error"}
			}
			if err := writeExportBytes(dst, []byte("\"\"\n")); err != nil {
				return &Error{Code: "write_error"}
			}
		} else if err := w.Write(row); err != nil {
			return &Error{Code: "write_error"}
		}
	}
	w.Flush()
	if w.Error() != nil {
		return &Error{Code: "write_error"}
	}
	return nil
}

func validateExport(d *Dataset) error {
	if d == nil || len(d.Header) == 0 {
		return &Error{Code: "invalid_header", Record: 1}
	}
	width := len(d.Header)
	if width > MaxColumns {
		return &Error{Code: "too_many_columns", Record: 1}
	}
	valid := false
	for _, v := range d.Header {
		if !utf8.ValidString(v) {
			return &Error{Code: "invalid_utf8", Record: 1}
		}
		if strings.TrimSpace(v) != "" {
			valid = true
		}
	}
	if !valid {
		return &Error{Code: "invalid_header", Record: 1}
	}
	if len(d.Rows) > MaxRows {
		return &Error{Code: "too_many_rows"}
	}
	if len(d.Rows) > MaxCells/width {
		return &Error{Code: "too_many_cells"}
	}
	for i, row := range d.Rows {
		if len(row) != width {
			return &Error{Code: "inconsistent_width", Record: i + 2}
		}
		for _, v := range row {
			if !utf8.ValidString(v) {
				return &Error{Code: "invalid_utf8", Record: i + 2}
			}
		}
	}
	return nil
}

// CleanedFilename derives the ASCII download name specified by the export contract.
func CleanedFilename(name string) string {
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	var b strings.Builder
	underscore := false
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteByte(byte(r))
			underscore = false
		} else if !underscore {
			b.WriteByte('_')
			underscore = true
		}
	}
	stem := strings.Trim(b.String(), "_")
	if len(stem) > 80 {
		stem = stem[:80]
	}
	if stem == "" {
		return "cleaned.csv"
	}
	return stem + "_cleaned.csv"
}

func writeExportBytes(dst io.Writer, p []byte) error {
	n, err := dst.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}
