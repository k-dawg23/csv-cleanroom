package csvdata

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeHeader(t *testing.T) {
	tests := []struct{ in, want string }{
		{"First Name", "first_name"}, {"EMAIL ADDRESS", "email_address"}, {"Order-Date", "order_date"}, {" Café Name ", "café_name"}, {"Product (ID)", "product_id"},
		{"__ A--  B___", "a_b"}, {"名 前", "名_前"}, {"A\u0301\u0302", "a\u0301\u0302"}, {"a!\u0301", "a"}, {"_\u0301a", "a"}, {"1\u0301a", "1a"}, {"\u0301a", "a"}, {"a-\u0301b", "a_b"}, {"a_\u0301b", "a_b"}, {"!!!", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := normalizeHeader(tt.in); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestProcessHeaderErrors(t *testing.T) {
	for _, tt := range []struct {
		header    []string
		code      string
		positions []int
	}{
		{[]string{"valid", "!!!"}, "empty_normalized_header", []int{2}},
		{[]string{"First Name", "first-name"}, "duplicate_normalized_header", []int{1, 2}},
	} {
		d := &Dataset{Header: tt.header, Rows: [][]string{{"a", "b"}}}
		before := append([]string(nil), d.Header...)
		got, report, err := Process(d, Options{NormalizeHeaders: true})
		var e *HeaderError
		if got != nil || report != nil || !errors.As(err, &e) || e.Code != tt.code || !reflect.DeepEqual(e.Columns, tt.positions) || !reflect.DeepEqual(d.Header, before) {
			t.Fatalf("unexpected failure: %v %#v", err, e)
		}
	}
	d := &Dataset{Header: []string{"", "same", "same"}}
	got, r, err := Process(d, Options{})
	if err != nil || !reflect.DeepEqual(got.Header, d.Header) || r.HeadersChanged.Applied || r.Missing.Columns[1].Position != 2 || r.Missing.Columns[1].OriginalHeading != "same" {
		t.Fatalf("off: %#v %#v %v", got, r, err)
	}
	d = &Dataset{Header: []string{"é", "e\u0301"}}
	if _, _, err := Process(d, Options{NormalizeHeaders: true}); err != nil {
		t.Fatal(err)
	}
}

func TestProcessOptionsAndOrdering(t *testing.T) {
	input := &Dataset{Header: []string{" First Name ", "Value"}, Rows: [][]string{{" a ", " "}, {"a", ""}, {"A", "NULL"}, {"a", "N/A"}, {"a", "-"}}}
	tests := []struct {
		name                                string
		options                             Options
		rows                                [][]string
		header                              []string
		trim, duplicates, missing, affected int
	}{
		{"defaults", Options{}, input.Rows, input.Header, 0, 0, 2, 2},
		{"trim", Options{TrimWhitespace: true}, [][]string{{"a", ""}, {"a", ""}, {"A", "NULL"}, {"a", "N/A"}, {"a", "-"}}, input.Header, 2, 0, 2, 2},
		{"dedup only", Options{RemoveDuplicates: true}, input.Rows, input.Header, 0, 0, 2, 2},
		{"headers only", Options{NormalizeHeaders: true}, input.Rows, []string{"first_name", "value"}, 0, 0, 2, 2},
		{"all", Options{TrimWhitespace: true, RemoveDuplicates: true, NormalizeHeaders: true}, [][]string{{"a", ""}, {"A", "NULL"}, {"a", "N/A"}, {"a", "-"}}, []string{"first_name", "value"}, 2, 1, 1, 1},
		{"all disabled", Options{DisableMissingAnalysis: true}, input.Rows, input.Header, 0, 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, r, err := Process(input, tt.options)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Rows, tt.rows) || !reflect.DeepEqual(got.Header, tt.header) || r.CellsTrimmed.Count != tt.trim || r.DuplicateRowsRemoved.Count != tt.duplicates || r.Missing.TotalCells != tt.missing || r.Missing.RowsWithMissing != tt.affected || r.OriginalRows != 5 || r.ProcessedRows != len(tt.rows) || r.OriginalColumns != 2 || r.ProcessedColumns != 2 || r.Missing.Applied == tt.options.DisableMissingAnalysis || r.CellsTrimmed.Applied != tt.options.TrimWhitespace || r.DuplicateRowsRemoved.Applied != tt.options.RemoveDuplicates || r.HeadersChanged.Applied != tt.options.NormalizeHeaders {
				t.Fatalf("result %#v report %#v", got, r)
			}
			if tt.options.NormalizeHeaders && r.HeadersChanged.Count != 2 {
				t.Fatal(r)
			}
			if r.Missing.Applied && (r.Missing.Columns[0].Count != 0 || r.Missing.Columns[1].Count != tt.missing) {
				t.Fatal(r.Missing)
			}
			got.Header[0] = "changed"
			got.Rows[0][0] = "changed"
			if input.Header[0] != " First Name " || input.Rows[0][0] != " a " {
				t.Fatal("source mutated")
			}
			again, r2, err := Process(input, tt.options)
			if err != nil {
				t.Fatal(err)
			}
			third, r3, err := Process(input, tt.options)
			if err != nil || !reflect.DeepEqual(again, third) || !reflect.DeepEqual(r2, r3) {
				t.Fatal("not deterministic")
			}
		})
	}
}

func TestTrimValues(t *testing.T) {
	for _, tt := range []struct {
		value, want string
		changed     int
	}{{" x", "x", 1}, {"x ", "x", 1}, {" \t", "", 1}, {"\u2003x\u00a0", "x", 1}, {"", "", 0}, {"x y", "x y", 0}} {
		t.Run(tt.value, func(t *testing.T) {
			d := &Dataset{Header: []string{" H "}, Rows: [][]string{{tt.value}}}
			got, r, err := Process(d, Options{TrimWhitespace: true})
			if err != nil || got.Rows[0][0] != tt.want || got.Header[0] != " H " || r.CellsTrimmed.Count != tt.changed {
				t.Fatalf("%#v %#v %v", got, r, err)
			}
		})
	}
}

func TestDuplicateBoundaries(t *testing.T) {
	rows := [][]string{{"a,b", "c"}, {"a", "b,c"}, {"a\nb", "c"}, {"a", "b\nc"}, {"", "x"}, {"", "x"}, {"A", "x"}, {"a", "x"}, {"a", "x"}}
	d := &Dataset{Header: []string{"a", "b"}, Rows: rows}
	got, r, err := Process(d, Options{RemoveDuplicates: true})
	if err != nil || r.DuplicateRowsRemoved.Count != 2 || !reflect.DeepEqual(got.Rows, [][]string{rows[0], rows[1], rows[2], rows[3], rows[4], rows[6], rows[7]}) {
		t.Fatalf("%#v %#v %v", got, r, err)
	}
}

func TestProcessBoundaries(t *testing.T) {
	for _, d := range []*Dataset{nil, {}, {Header: []string{" "}}, {Header: []string{"a"}, Rows: [][]string{{"a", "b"}}}, {Header: make([]string, MaxColumns+1)}, {Header: []string{"a"}, Rows: make([][]string, MaxRows+1)}} {
		if _, _, err := Process(d, Options{}); err == nil {
			t.Fatalf("expected error for %#v", d)
		}
	}
	d, err := Parse(strings.NewReader("name\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, r, err := Process(d, Options{})
	if err != nil || len(got.Rows) != 0 || r.Missing.Columns[0].Count != 0 {
		t.Fatalf("%#v %#v %v", got, r, err)
	}
	d, err = Parse(strings.NewReader("h\n\"a\r\nb\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, _, err = Process(d, Options{})
	if err != nil || got.Rows[0][0] != "a\nb" {
		t.Fatal("parser regression", err)
	}
}
