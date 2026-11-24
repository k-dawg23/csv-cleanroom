package csvdata

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestExportRoundTrip(t *testing.T) {
	tests := []struct {
		name, input, want string
		opts              Options
	}{
		{"existing empty", "Name\n\"\"\nAlice\n", "Name\n\"\"\nAlice\n", Options{}},
		{"trimmed empty", "Name\n\"   \"\nAlice\n", "Name\n\"\"\nAlice\n", Options{TrimWhitespace: true}},
		{"repeated empty", "Name\n\"\"\n\"\"\nAlice\n", "Name\n\"\"\n\"\"\nAlice\n", Options{}},
		{"header only", "a,b\n", "a,b\n", Options{}},
		{"fields", "h1,h2,h3\r\n\"a,b\",\"a\"\"b\",\r\n\"line\r\nline\",\"a\rb\",猫\r\n", "h1,h2,h3\n\"a,b\",\"a\"\"b\",\n\"line\nline\",\"a\rb\",猫\n", Options{}},
		{"whitespace", "h\n   \n", "h\n\"   \"\n", Options{}},
		{"bom as content", "\xef\xbb\xbf\xef\xbb\xbfName\nvalue\n", "\"\xef\xbb\xbfName\"\nvalue\n", Options{}},
		{"formulas", "=head,+head,-head,@head\n=1,+2,-3,@x\n", "=head,+head,-head,@head\n=1,+2,-3,@x\n", Options{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original, err := Parse(strings.NewReader(tt.input))
			if err != nil {
				t.Fatal(err)
			}
			origCopy := cloneDataset(original)
			processed, _, err := Process(original, tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			copy := cloneDataset(processed)
			out, err := Export(processed)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tt.want {
				t.Fatalf("got %q want %q", out, tt.want)
			}
			again, err := Export(processed)
			if err != nil || !bytes.Equal(out, again) {
				t.Fatal("non-deterministic", err)
			}
			parsed, err := Parse(bytes.NewReader(out))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(parsed.Header, processed.Header) || !(len(parsed.Rows) == 0 && len(processed.Rows) == 0 || reflect.DeepEqual(parsed.Rows, processed.Rows)) {
				t.Fatalf("round trip: %#v != %#v", parsed, processed)
			}
			if !reflect.DeepEqual(original, origCopy) || !reflect.DeepEqual(processed, copy) {
				t.Fatal("dataset mutated")
			}
		})
	}
}
func cloneDataset(d *Dataset) *Dataset {
	c := &Dataset{Header: append([]string(nil), d.Header...)}
	if d.Rows != nil {
		c.Rows = make([][]string, len(d.Rows))
		for i, r := range d.Rows {
			c.Rows[i] = append([]string(nil), r...)
		}
	}
	return c
}

func TestExportProcessingOptions(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts Options
		want string
	}{
		{"none", Options{}, "\" First Name \",Value\n\" a \",\" \"\na,\na,\n"},
		{"trim", Options{TrimWhitespace: true}, "\" First Name \",Value\na,\na,\na,\n"},
		{"dedup", Options{RemoveDuplicates: true}, "\" First Name \",Value\n\" a \",\" \"\na,\n"},
		{"headers", Options{NormalizeHeaders: true}, "first_name,value\n\" a \",\" \"\na,\na,\n"},
		{"all", Options{NormalizeHeaders: true, TrimWhitespace: true, RemoveDuplicates: true}, "first_name,value\na,\n"},
		{"analysis disabled", Options{DisableMissingAnalysis: true}, "\" First Name \",Value\n\" a \",\" \"\na,\na,\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original, err := Parse(strings.NewReader(" First Name ,Value\n a , \na,\na,\n"))
			if err != nil {
				t.Fatal(err)
			}
			before := cloneDataset(original)
			d, _, err := Process(original, tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			processed := cloneDataset(d)
			out, err := Export(d)
			if err != nil || string(out) != tt.want {
				t.Fatalf("%q %v", out, err)
			}
			parsed, err := Parse(bytes.NewReader(out))
			if err != nil || !reflect.DeepEqual(parsed, d) {
				t.Fatalf("%#v %v", parsed, err)
			}
			if !reflect.DeepEqual(original, before) || !reflect.DeepEqual(d, processed) {
				t.Fatal("mutation")
			}
		})
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("secret") }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }
func TestExportValidation(t *testing.T) {
	cases := []struct {
		name string
		d    *Dataset
		code string
	}{
		{"nil", nil, "invalid_header"}, {"missing", &Dataset{}, "invalid_header"}, {"blank", &Dataset{Header: []string{" ", ""}}, "invalid_header"},
		{"columns", &Dataset{Header: make([]string, MaxColumns+1)}, "too_many_columns"},
		{"rows", &Dataset{Header: []string{"h"}, Rows: make([][]string, MaxRows+1)}, "too_many_rows"},
		{"cells", &Dataset{Header: make([]string, 20), Rows: make([][]string, 50001)}, "too_many_cells"},
		{"width", &Dataset{Header: []string{"secret"}, Rows: [][]string{{"secret", "bad"}}}, "inconsistent_width"},
		{"utf8 header", &Dataset{Header: []string{"secret\xff"}}, "invalid_utf8"},
		{"utf8 data", &Dataset{Header: []string{"h"}, Rows: [][]string{{"secret\xff"}}}, "invalid_utf8"},
	}
	cases[5].d.Header[0] = "h"
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := WriteCSV(&out, tt.d)
			var e *Error
			if !errors.As(err, &e) || e.Code != tt.code || strings.Contains(err.Error(), "secret") || out.Len() != 0 {
				t.Fatalf("%v %q", err, out.String())
			}
		})
	}
	for _, w := range []io.Writer{failWriter{}, shortWriter{}, nil} {
		err := WriteCSV(w, &Dataset{Header: []string{"h"}})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe writer error %v", err)
		}
	}
	// Error on a later buffered record and on the special empty-record path.
	for _, d := range []*Dataset{{Header: []string{"h"}, Rows: [][]string{{"x"}}}, {Header: []string{"h"}, Rows: [][]string{{""}}}} {
		w := &limitedWriter{remaining: 2}
		if err := WriteCSV(w, d); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("write failure %v", err)
		}
	}
}

type limitedWriter struct{ remaining int }

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, errors.New("secret")
	}
	w.remaining -= len(p)
	return len(p), nil
}
func TestExportLimits(t *testing.T) {
	for _, tt := range []struct {
		name       string
		cols, rows int
	}{{"columns", MaxColumns, 1}, {"rows and cells", 10, MaxRows}, {"rows", 1, MaxRows}} {
		t.Run(tt.name, func(t *testing.T) {
			header := make([]string, tt.cols)
			header[0] = "h"
			row := make([]string, tt.cols)
			rows := make([][]string, tt.rows)
			for i := range rows {
				rows[i] = row
			}
			d := &Dataset{Header: header, Rows: rows}
			if err := WriteCSV(io.Discard, d); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestExportHasNoInputByteLimit(t *testing.T) {
	d := &Dataset{Header: []string{"h"}, Rows: [][]string{{strings.Repeat("x", MaxFileBytes+1)}}}
	if err := WriteCSV(io.Discard, d); err != nil {
		t.Fatal(err)
	}
}

func TestCleanedFilename(t *testing.T) {
	for _, tt := range []struct{ input, want string }{{"dir\\sub/file.csv", "file_cleaned.csv"}, {".csv", "cleaned.csv"}, {"é \r\n\"X.csv", "X_cleaned.csv"}, {"a__b.c.csv", "a_b_c_cleaned.csv"}, {"---.csv", "---_cleaned.csv"}, {strings.Repeat("a", 81) + ".csv", strings.Repeat("a", 80) + "_cleaned.csv"}, {"name", "name_cleaned.csv"}} {
		t.Run(tt.input, func(t *testing.T) {
			if got := CleanedFilename(tt.input); got != tt.want {
				t.Fatalf("%q != %q", got, tt.want)
			}
		})
	}
}
