package csvdata

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestParseRecords(t *testing.T) {
	tests := []struct {
		name, input, code string
		header            []string
		rows              [][]string
	}{
		{"utf8", "naïve,名\ncafé,猫\n", "", []string{"naïve", "名"}, [][]string{{"café", "猫"}}},
		{"invalid utf8", "h\n\xff", "invalid_utf8", nil, nil},
		{"bom", "\xef\xbb\xbfh\nx\n", "", []string{"h"}, [][]string{{"x"}}},
		{"second bom", "\xef\xbb\xbf\xef\xbb\xbfh\n", "", []string{"\ufeffh"}, nil},
		{"empty", "", "empty_input", nil, nil},
		{"bom only", "\xef\xbb\xbf", "empty_input", nil, nil},
		{"blank lines", "\n\r\n\n", "empty_input", nil, nil},
		{"ignored physical lines", "\nh\n\nvalue\n\r\n", "", []string{"h"}, [][]string{{"value"}}},
		{"whitespace header", " \t\n", "invalid_header", nil, nil},
		{"whitespace data", "h\n   \n", "", []string{"h"}, [][]string{{"   "}}},
		{"header only", "h1,h2\n", "", []string{"h1", "h2"}, nil},
		{"single column", "h\nx\n", "", []string{"h"}, [][]string{{"x"}}},
		{"quoted comma", "h1,h2\n\"a,b\",c\n", "", []string{"h1", "h2"}, [][]string{{"a,b", "c"}}},
		{"embedded newline", "h\n\"a\nb\"\n", "", []string{"h"}, [][]string{{"a\nb"}}},
		{"crlf normalization", "h1,h2\r\n\"a\r\nb\",c\r\n", "", []string{"h1", "h2"}, [][]string{{"a\nb", "c"}}},
		{"bare cr", "h\n\"a\rb\"\n", "", []string{"h"}, [][]string{{"a\rb"}}},
		{"malformed quote", "h\nsecret\"bad\n", "invalid_csv", nil, nil},
		{"width", "h1,h2\na\n", "inconsistent_width", nil, nil},
		{"trailing empty", "h1,h2\na,\n", "", []string{"h1", "h2"}, [][]string{{"a", ""}}},
		{"blank and duplicate headers", ",same,same\n1,2,3\n", "", []string{"", "same", "same"}, [][]string{{"1", "2", "3"}}},
		{"all blank headers", " ,\t\n", "invalid_header", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(tt.input))
			if tt.code != "" {
				var e *Error
				if !errors.As(err, &e) || e.Code != tt.code {
					t.Fatalf("got %v, want %s", err, tt.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Header, tt.header) || !reflect.DeepEqual(got.Rows, tt.rows) {
				t.Fatalf("got %#v", got)
			}
		})
	}
}

func TestLimits(t *testing.T) {
	header := func(n int) string { return strings.TrimSuffix(strings.Repeat("h,", n), ",") + "\n" }
	row := func(n int) string { return strings.Repeat(",", n-1) + "\n" }
	tests := []struct{ name, input, code string }{
		{"file exact", "h\n" + strings.Repeat("x", MaxFileBytes-2), ""},
		{"file excess", "h\n" + strings.Repeat("x", MaxFileBytes-1), "file_too_large"},
		{"rows exact", "h\n" + strings.Repeat("x\n", MaxRows), ""},
		{"rows excess", "h\n" + strings.Repeat("x\n", MaxRows+1), "too_many_rows"},
		{"columns exact", header(MaxColumns), ""},
		{"columns excess", header(MaxColumns + 1), "too_many_columns"},
		{"cells exact", header(10) + strings.Repeat(row(10), 100000), ""},
		{"cells excess", header(20) + strings.Repeat(row(20), 50001), "too_many_cells"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.input))
			if tt.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var e *Error
			if !errors.As(err, &e) || e.Code != tt.code {
				t.Fatalf("got %v, want %s", err, tt.code)
			}
		})
	}
}

func TestPreview(t *testing.T) {
	for _, n := range []int{0, 19, 20, 21} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			d, err := Parse(strings.NewReader("a,b\n" + strings.Repeat("x,\n", n)))
			if err != nil {
				t.Fatal(err)
			}
			p := d.Preview()
			if p.RowCount != n || p.ColumnCount != 2 || len(p.Rows) != min(n, 20) {
				t.Fatalf("bad preview: %#v", p)
			}
			if n > 0 {
				if p.Rows[0][1] != "" {
					t.Fatal("empty field lost")
				}
				p.Rows[0][0] = "changed"
				if d.Rows[0][0] != "x" {
					t.Fatal("preview aliases source")
				}
			}
			p.Header[0] = "changed"
			if d.Header[0] != "a" {
				t.Fatal("header aliases source")
			}
		})
	}
}

type badReader struct{}

func (badReader) Read([]byte) (int, error) { return 0, errors.New("private CSV contents") }
func TestSafeErrors(t *testing.T) {
	for _, input := range []io.Reader{badReader{}, strings.NewReader("h\nprivate\"contents\n"), strings.NewReader("h,x\nprivate\n")} {
		_, err := Parse(input)
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	_, err := Parse(strings.NewReader("h,x\na,b\nsecret\n"))
	var e *Error
	if !errors.As(err, &e) || e.Record != 3 {
		t.Fatalf("bad record: %v", err)
	}
}
