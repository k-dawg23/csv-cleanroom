package csvdata

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode"
)

// Options has the specification's defaults as its zero value.
type Options struct {
	NormalizeHeaders       bool
	TrimWhitespace         bool
	RemoveDuplicates       bool
	DisableMissingAnalysis bool
}

type CountResult struct {
	Applied bool
	Count   int
}
type ColumnMissing struct {
	Position        int
	OriginalHeading string
	Count           int
}
type MissingResult struct {
	Applied                     bool
	Columns                     []ColumnMissing
	TotalCells, RowsWithMissing int
}
type Report struct {
	OriginalRows, OriginalColumns                      int
	ProcessedRows, ProcessedColumns                    int
	HeadersChanged, CellsTrimmed, DuplicateRowsRemoved CountResult
	Missing                                            MissingResult
}

// HeaderError identifies invalid normalized headings without exposing their contents.
type HeaderError struct {
	Code    string
	Columns []int
}

func (e *HeaderError) Error() string { return fmt.Sprintf("%s at columns %v", e.Code, e.Columns) }

// Process builds an isolated snapshot and report. No partial result is returned on error.
func Process(original *Dataset, options Options) (*Dataset, *Report, error) {
	return process(original, options, nil)
}

// ProcessWithCheckpoint is an internal integration-test seam. Production uses Process.
func ProcessWithCheckpoint(original *Dataset, options Options, checkpoint func()) (*Dataset, *Report, error) {
	return process(original, options, checkpoint)
}
func process(original *Dataset, options Options, checkpoint func()) (*Dataset, *Report, error) {
	if original == nil || len(original.Header) == 0 {
		return nil, nil, &Error{Code: "invalid_header", Record: 1}
	}
	width := len(original.Header)
	if width > MaxColumns {
		return nil, nil, &Error{Code: "too_many_columns", Record: 1}
	}
	valid := false
	for _, h := range original.Header {
		if strings.TrimSpace(h) != "" {
			valid = true
			break
		}
	}
	if !valid {
		return nil, nil, &Error{Code: "invalid_header", Record: 1}
	}
	if len(original.Rows) > MaxRows {
		return nil, nil, &Error{Code: "too_many_rows"}
	}
	if len(original.Rows) > MaxCells/width {
		return nil, nil, &Error{Code: "too_many_cells"}
	}
	for i, row := range original.Rows {
		if len(row) != width {
			return nil, nil, &Error{Code: "inconsistent_width", Record: i + 2}
		}
	}
	result := &Dataset{Header: append([]string(nil), original.Header...), Rows: make([][]string, 0, len(original.Rows))}
	report := &Report{OriginalRows: len(original.Rows), OriginalColumns: width, ProcessedColumns: width,
		HeadersChanged: CountResult{Applied: options.NormalizeHeaders}, CellsTrimmed: CountResult{Applied: options.TrimWhitespace}, DuplicateRowsRemoved: CountResult{Applied: options.RemoveDuplicates}}
	if options.NormalizeHeaders {
		seen := make(map[string]int, width)
		for i, h := range original.Header {
			normalized := normalizeHeader(h)
			if normalized == "" {
				return nil, nil, &HeaderError{Code: "empty_normalized_header", Columns: []int{i + 1}}
			}
			if first, ok := seen[normalized]; ok {
				return nil, nil, &HeaderError{Code: "duplicate_normalized_header", Columns: []int{first, i + 1}}
			}
			seen[normalized] = i + 1
			result.Header[i] = normalized
			if normalized != h {
				report.HeadersChanged.Count++
			}
		}
	}
	var seenRows map[string]struct{}
	if options.RemoveDuplicates {
		seenRows = make(map[string]struct{}, len(original.Rows))
	}
	for rowIndex, source := range original.Rows {
		row := append([]string(nil), source...)
		if options.TrimWhitespace {
			for i, v := range row {
				trimmed := strings.TrimSpace(v)
				if trimmed != v {
					report.CellsTrimmed.Count++
					row[i] = trimmed
				}
			}
		}
		if options.RemoveDuplicates {
			key := rowKey(row)
			if _, exists := seenRows[key]; exists {
				report.DuplicateRowsRemoved.Count++
				continue
			}
			seenRows[key] = struct{}{}
		}
		result.Rows = append(result.Rows, row)
		if rowIndex == 0 && checkpoint != nil {
			checkpoint()
		}
	}
	report.ProcessedRows = len(result.Rows)
	if !options.DisableMissingAnalysis {
		report.Missing.Applied = true
		report.Missing.Columns = make([]ColumnMissing, width)
		for i, h := range original.Header {
			report.Missing.Columns[i] = ColumnMissing{Position: i + 1, OriginalHeading: h}
		}
		for _, row := range result.Rows {
			missing := false
			for i, v := range row {
				if strings.TrimSpace(v) == "" {
					report.Missing.Columns[i].Count++
					report.Missing.TotalCells++
					missing = true
				}
			}
			if missing {
				report.Missing.RowsWithMissing++
			}
		}
	}
	return result, report, nil
}

func rowKey(row []string) string {
	var b strings.Builder
	var buf [binary.MaxVarintLen64]byte
	for _, v := range row {
		n := binary.PutUvarint(buf[:], uint64(len(v)))
		b.Write(buf[:n])
		b.WriteString(v)
	}
	return b.String()
}

func normalizeHeader(header string) string {
	var b strings.Builder
	associated := false
	underscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(header)) {
		switch {
		case unicode.IsSpace(r) || r == '-':
			if b.Len() > 0 && !underscore {
				b.WriteByte('_')
				underscore = true
			}
			associated = false
		case unicode.IsLetter(r):
			b.WriteRune(r)
			associated = true
			underscore = false
		case unicode.IsMark(r):
			if associated {
				b.WriteRune(r)
				underscore = false
			}
		case unicode.IsNumber(r):
			b.WriteRune(r)
			associated = false
			underscore = false
		case r == '_':
			if b.Len() > 0 && !underscore {
				b.WriteByte('_')
				underscore = true
			}
			associated = false
		default:
			associated = false
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}
