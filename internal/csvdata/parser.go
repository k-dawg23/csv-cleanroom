// Package csvdata parses bounded CSV input into original logical records.
package csvdata

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	MaxFileBytes   = 10 * 1024 * 1024
	MaxRows        = 100000
	MaxColumns     = 128
	MaxCells       = 1000000
	MaxPreviewRows = 20
)

// Error is a safe diagnostic: Record is a 1-based logical record number when known.
// Its message never includes source data or an underlying reader/parser error.
type Error struct {
	Code   string
	Record int
}

func (e *Error) Error() string {
	if e.Record > 0 {
		return fmt.Sprintf("%s at record %d", e.Code, e.Record)
	}
	return e.Code
}

// Dataset holds the original parsed values. Callers should treat it as immutable.
type Dataset struct {
	Header []string
	Rows   [][]string
}

// Preview includes original dimensions and a detached selection of data rows.
type Preview struct {
	Header                []string
	Rows                  [][]string
	RowCount, ColumnCount int
}

func (d *Dataset) Preview() Preview {
	p := Preview{Header: append([]string(nil), d.Header...), RowCount: len(d.Rows), ColumnCount: len(d.Header)}
	n := min(len(d.Rows), MaxPreviewRows)
	p.Rows = make([][]string, n)
	for i := range n {
		p.Rows[i] = append([]string(nil), d.Rows[i]...)
	}
	return p
}

// Parse consumes a complete raw file, checks its size and encoding, then parses
// logical records. Read errors and CSV errors are deliberately not exposed.
func Parse(src io.Reader) (*Dataset, error) {
	raw, err := io.ReadAll(io.LimitReader(src, MaxFileBytes+1))
	if err != nil {
		return nil, &Error{Code: "read_error"}
	}
	if len(raw) > MaxFileBytes {
		return nil, &Error{Code: "file_too_large"}
	}
	if !utf8.Valid(raw) {
		return nil, &Error{Code: "invalid_utf8"}
	}
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	reader := csv.NewReader(bytes.NewReader(raw))
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err == io.EOF {
		return nil, &Error{Code: "empty_input"}
	}
	if err != nil {
		return nil, csvError(err, 1)
	}
	if len(header) > MaxColumns {
		return nil, &Error{Code: "too_many_columns", Record: 1}
	}
	valid := false
	for _, h := range header {
		if strings.TrimSpace(h) != "" {
			valid = true
			break
		}
	}
	if !valid {
		return nil, &Error{Code: "invalid_header", Record: 1}
	}
	reader.FieldsPerRecord = len(header)
	result := &Dataset{Header: header}
	for {
		row, err := reader.Read()
		if err == io.EOF {
			return result, nil
		}
		record := len(result.Rows) + 2
		if err != nil {
			return nil, csvError(err, record)
		}
		count := len(result.Rows) + 1
		if count > MaxRows {
			return nil, &Error{Code: "too_many_rows", Record: record}
		}
		// Division avoids multiplication overflow and counts empty fields as cells.
		if count > MaxCells/len(header) {
			return nil, &Error{Code: "too_many_cells", Record: record}
		}
		result.Rows = append(result.Rows, row)
	}
}
func csvError(err error, record int) error {
	code := "invalid_csv"
	if errors.Is(err, csv.ErrFieldCount) {
		code = "inconsistent_width"
	}
	return &Error{Code: code, Record: record}
}
