package xlsx

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// minimalWorkbookPart is entered into the archive last (see
// buildFallbackNamingWorkbook), after every worksheet part, so that
// resolveSheet has no workbook metadata yet when those worksheets are
// reached and must fall back to a name/index derived from the archive
// itself. sheetCount controls how many <sheet> entries it declares, each
// pointing at rIdN -> xl/worksheets/sheetN.xml.
func minimalWorkbookPart(sheetCount int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	for i := 1; i <= sheetCount; i++ {
		fmt.Fprintf(&b, `<sheet name="Sheet%d" sheetId="%d" r:id="rId%d"/>`, i, i, i)
	}
	b.WriteString(`</sheets></workbook>`)
	return b.String()
}

const rootRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`

func workbookRelsXML(sheetCount int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i := 1; i <= sheetCount; i++ {
		fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i, i)
	}
	b.WriteString(`</Relationships>`)
	return b.String()
}

func textCell(ref, text string) string {
	return `<c r="` + ref + `" t="inlineStr"><is><t>` + text + `</t></is></c>`
}

func numberCell(ref, v string) string {
	return `<c r="` + ref + `"><v>` + v + `</v></c>`
}

func boolCell(ref string, v bool) string {
	val := "0"
	if v {
		val = "1"
	}
	return `<c r="` + ref + `" t="b"><v>` + val + `</v></c>`
}

// formulaCellNoCache emits a formula cell with no cached <v>, matching a
// writer that writes formulas without evaluating them.
func formulaCellNoCache(ref, formula string) string {
	return `<c r="` + ref + `"><f>` + formula + `</f></c>`
}

func worksheetXML(rows ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for i, cells := range rows {
		fmt.Fprintf(&b, `<row r="%d">%s</row>`, i+1, cells)
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String()
}

// buildFallbackNamingWorkbook hand-builds a minimal, valid .xlsx archive
// (via the stdlib archive/zip) whose worksheet parts are written before
// the workbook part and its relationships — a legal ZIP entry order that
// real writers (xuri/excelize, openpyxl, and WPS Office have all been
// observed doing exactly this, as their normal behavior rather than an
// edge case) produce routinely, not just a writer that can't seek back
// to finish xl/workbook.xml until every sheet has been streamed out.
// This exercises the fallback (metadata not yet available) sheet-naming
// path in TestReader_FallbackSheetNaming below.
func buildFallbackNamingWorkbook(t *testing.T, sheets [][]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	write := func(name, content string) {
		t.Helper()
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("creating zip entry %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("writing zip entry %q: %v", name, err)
		}
	}

	for i, rows := range sheets {
		write(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), worksheetXML(rows...))
	}
	write("_rels/.rels", rootRelsXML)
	write("xl/workbook.xml", minimalWorkbookPart(len(sheets)))
	write("xl/_rels/workbook.xml.rels", workbookRelsXML(len(sheets)))

	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip writer: %v", err)
	}

	return buf.Bytes()
}

func collectRows(t *testing.T, sheet *Sheet) [][]string {
	t.Helper()

	rows := sheet.Rows()

	var got [][]string
	for rows.Next() {
		// Copy: Columns' backing array is reused across Next calls.
		got = append(got, append([]string(nil), rows.Columns()...))
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("row iteration failed: %v", err)
	}

	return got
}

// TestReader_FallbackSheetNaming cross-checks this general Reader against
// a hand-built workbook (see buildFallbackNamingWorkbook) whose worksheet
// parts precede the workbook part, the same unconventional but legal
// ordering a writer produces when it can't seek back to finish
// xl/workbook.xml until every sheet has been streamed out. That exercises
// the fallback (metadata not yet available) sheet-naming path, not the
// metadata-driven one, alongside inline-string, numeric, boolean, and
// uncached-formula cells.
func TestReader_FallbackSheetNaming(t *testing.T) {
	sheet1 := []string{
		textCell("A1", "name") + textCell("B1", "count") + textCell("C1", "active") + textCell("D1", "formula"),
		textCell("A2", "Alice") + numberCell("B2", "3") + boolCell("C2", true) + formulaCellNoCache("D2", "1+1"),
	}
	sheet2 := []string{
		textCell("A1", "Bob") + numberCell("B1", "4") + boolCell("C1", false) + textCell("D1", "plain"),
		textCell("A2", "Carol") + numberCell("B2", "5") + boolCell("C2", true) + textCell("D2", "42"), // leading-apostrophe-style literal, already stripped
	}

	data := buildFallbackNamingWorkbook(t, [][]string{sheet1, sheet2}) // forces a split into two 2-row sheets

	rd, err := OpenReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	wantSheets := []struct {
		name string
		rows [][]string
	}{
		{"Sheet1", [][]string{
			{"name", "count", "active", "formula"},
			{"Alice", "3", "TRUE", "=1+1"},
		}},
		{"Sheet2", [][]string{
			{"Bob", "4", "FALSE", "plain"},
			{"Carol", "5", "TRUE", "42"}, // leading-apostrophe stripped, not re-escaped
		}},
	}

	for i, want := range wantSheets {
		sheet, err := rd.NextSheet()
		if err != nil {
			t.Fatalf("NextSheet: %v", err)
		}
		if sheet == nil {
			t.Fatalf("NextSheet returned nil at sheet %d, want %q", i+1, want.name)
		}
		if sheet.Name != want.name {
			t.Errorf("sheet %d: Name = %q, want %q", i+1, sheet.Name, want.name)
		}
		if sheet.Index != i+1 {
			t.Errorf("sheet %d: Index = %d, want %d", i+1, sheet.Index, i+1)
		}

		got := collectRows(t, sheet)
		if len(got) != len(want.rows) {
			t.Fatalf("sheet %d: got %d rows, want %d", i+1, len(got), len(want.rows))
		}
		for r, row := range got {
			if !equalStrings(row, want.rows[r]) {
				t.Errorf("sheet %d row %d: got %v, want %v", i+1, r, row, want.rows[r])
			}
		}
	}

	sheet, err := rd.NextSheet()
	if err != nil {
		t.Fatalf("NextSheet at end: %v", err)
	}
	if sheet != nil {
		t.Fatalf("NextSheet at end: got %+v, want nil", sheet)
	}
}

// TestRowIterator_Number checks Number against a worksheet with a gap in
// its row numbers (row "5" following row "1") and a row with no r
// attribute at all, which must fall back to one past the previous row's
// number (lastNumber + 1) rather than 0 or the row's physical position.
func TestRowIterator_Number(t *testing.T) {
	worksheet := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` +
		`<row r="1">` + textCell("A1", "first") + `</row>` +
		`<row r="5">` + textCell("A5", "fifth") + `</row>` +
		`<row>` + textCell("A6", "unnumbered") + `</row>` +
		`</sheetData></worksheet>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		t.Fatalf("creating zip entry: %v", err)
	}
	if _, err := w.Write([]byte(worksheet)); err != nil {
		t.Fatalf("writing zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip writer: %v", err)
	}

	rd, err := OpenReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	sheet, err := rd.NextSheet()
	if err != nil {
		t.Fatalf("NextSheet: %v", err)
	}
	if sheet == nil {
		t.Fatal("NextSheet returned nil, want a sheet")
	}

	want := []struct {
		number int
		text   string
	}{
		{1, "first"},
		{5, "fifth"},
		{6, "unnumbered"}, // no r attribute: one past the previous row's number
	}

	rows := sheet.Rows()
	for i, w := range want {
		if !rows.Next() {
			t.Fatalf("row %d: Next() = false, want true (err: %v)", i, rows.Err())
		}
		if rows.Number() != w.number {
			t.Errorf("row %d: Number() = %d, want %d", i, rows.Number(), w.number)
		}
		if got := rows.Columns(); len(got) != 1 || got[0] != w.text {
			t.Errorf("row %d: Columns() = %v, want [%q]", i, got, w.text)
		}
	}
	if rows.Next() {
		t.Fatalf("Next() = true after last row, want false")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReader_LibreOfficeMixed_RejectsSharedStrings uses a real,
// non-self-produced workbook (generated via LibreOffice headless — see
// testdata/generate.sh) that mixes text and numeric cells. Every string
// cell LibreOffice writes uses xl/sharedStrings.xml, which this reader
// deliberately doesn't support, so reading the header row (all text)
// must fail with a clear, specific error rather than silently
// misreading the shared-string index as literal text.
func TestReader_LibreOfficeMixed_RejectsSharedStrings(t *testing.T) {
	f, err := os.Open("testdata/libreoffice_mixed.xlsx")
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer f.Close()

	rd, err := OpenReader(f)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	sheet, err := rd.NextSheet()
	if err != nil {
		t.Fatalf("NextSheet: %v", err)
	}
	if sheet == nil {
		t.Fatal("NextSheet returned nil, want a sheet")
	}
	if sheet.Name != "sample" {
		t.Errorf("Name = %q, want %q (from the workbook part, not a fallback)", sheet.Name, "sample")
	}

	rows := sheet.Rows()
	if rows.Next() {
		t.Fatalf("Next() succeeded on a shared-string cell, got columns %v", rows.Columns())
	}

	err = rows.Err()
	if err == nil {
		t.Fatal("Err() = nil, want an error about shared strings")
	}
	if !strings.Contains(err.Error(), "shared strings") {
		t.Errorf("Err() = %v, want it to mention shared strings", err)
	}
}

// TestReader_LibreOfficeNumeric_SparseRows uses a real, non-self-produced,
// text-free workbook (see testdata/generate.sh) where LibreOffice both
// omits xl/sharedStrings.xml entirely (nothing references it) and — more
// importantly — omits blank cells from a row's XML rather than writing
// them empty, producing genuinely sparse rows this reader must gap-fill
// using each cell's r attribute.
func TestReader_LibreOfficeNumeric_SparseRows(t *testing.T) {
	f, err := os.Open("testdata/libreoffice_numeric.xlsx")
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer f.Close()

	rd, err := OpenReader(f)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	sheet, err := rd.NextSheet()
	if err != nil {
		t.Fatalf("NextSheet: %v", err)
	}
	if sheet == nil {
		t.Fatal("NextSheet returned nil, want a sheet")
	}
	if sheet.Name != "numeric" {
		t.Errorf("Name = %q, want %q", sheet.Name, "numeric")
	}
	if sheet.Index != 1 {
		t.Errorf("Index = %d, want 1", sheet.Index)
	}

	want := [][]string{
		{"1", "2", "3", "4"},
		{"5", "", "7"},        // B3 gap-filled; D3's trailing omission can't be inferred without a cell present after it
		{"9", "10", "", "12"}, // C4 gap-filled between B4 and D4
	}

	got := collectRows(t, sheet)
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
	}
	for i, row := range got {
		if !equalStrings(row, want[i]) {
			t.Errorf("row %d: got %v, want %v", i+1, row, want[i])
		}
	}

	next, err := rd.NextSheet()
	if err != nil {
		t.Fatalf("NextSheet at end: %v", err)
	}
	if next != nil {
		t.Fatalf("NextSheet at end: got %+v, want nil", next)
	}
}

// TestReader_RejectsGarbageInput checks truncated input shorter than a
// single ZIP local file header: too short to be any kind of valid
// archive, so this must surface as a real error rather than silently
// reporting no sheets. Content long enough to fill a would-be header but
// still not starting with the local-file-header signature is, by
// contrast, indistinguishable from an empty archive's central directory
// without ever looking at it (which this reader deliberately never
// does) — see TestReader_EmptyInput for that legitimately sheet-less
// case.
func TestReader_RejectsGarbageInput(t *testing.T) {
	rd, err := OpenReader(strings.NewReader("not a zip file at all"))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	sheet, err := rd.NextSheet()
	if err == nil {
		t.Fatalf("NextSheet on garbage input = (%+v, nil), want an error", sheet)
	}
}

// TestReader_EmptyInput checks that a genuinely empty input (as opposed
// to non-empty garbage — see TestReader_RejectsGarbageInput) is treated
// as an archive with no entries, not an error: NextSheet just reports no
// sheets, same as it would for a real, fully-consumed workbook.
func TestReader_EmptyInput(t *testing.T) {
	rd, err := OpenReader(strings.NewReader(""))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	sheet, err := rd.NextSheet()
	if err != nil {
		t.Fatalf("NextSheet on empty input: %v", err)
	}
	if sheet != nil {
		t.Fatalf("NextSheet on empty input = %+v, want nil", sheet)
	}
}

func TestLooksLikeWorksheetPart(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"xl/worksheets/sheet1.xml", true},
		{"xl/worksheets/Sheet7.xml", true},
		{"xl/worksheets/_rels/sheet1.xml.rels", false},
		{"xl/worksheets/", false},
		{"xl/worksheets", false},
		{"xl/styles.xml", false},
		{"xl/worksheets/sheet1.xml.rels", false}, // direct child, but doesn't end in .xml
	}

	for _, c := range cases {
		if got := looksLikeWorksheetPart(c.name); got != c.want {
			t.Errorf("looksLikeWorksheetPart(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseCellColumn(t *testing.T) {
	cases := []struct {
		ref     string
		want    int
		wantErr bool
	}{
		{"A1", 1, false},
		{"Z1", 26, false},
		{"AA1", 27, false},
		{"AC12", 29, false},
		{"c5", 3, false},
		{"ac12", 29, false},
		{"1", 0, true},
		{"", 0, true},
	}

	for _, c := range cases {
		got, err := parseCellColumn(c.ref)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseCellColumn(%q) = %d, want error", c.ref, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCellColumn(%q) unexpected error: %v", c.ref, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseCellColumn(%q) = %d, want %d", c.ref, got, c.want)
		}
	}
}

var rawFormatContext = &formatContext{raw: true}

func TestBuildRow_GapFilling(t *testing.T) {
	cells := []generalWorksheetCell{
		{R: "A1", T: "n", V: "1"},
		{R: "C1", T: "n", V: "3"},
	}

	number, columns, err := buildRow("1", cells, rawFormatContext)
	if err != nil {
		t.Fatalf("buildRow: %v", err)
	}
	if number != 1 {
		t.Errorf("number = %d, want 1", number)
	}

	want := []string{"1", "", "3"}
	if !equalStrings(columns, want) {
		t.Errorf("columns = %v, want %v", columns, want)
	}
}

func TestGeneralCellText(t *testing.T) {
	// Style index 1 -> numFmtId 3 (built-in "#,##0"), for the formatting
	// case below; every other case uses the default style 0 (General).
	formatted := &formatContext{ready: true, cellXfs: []int{0, 3}}

	cases := []struct {
		name    string
		cell    generalWorksheetCell
		fc      *formatContext
		want    string
		wantErr bool
	}{
		{"number", generalWorksheetCell{T: "n", V: "42"}, rawFormatContext, "42", false},
		{"bare number (t omitted)", generalWorksheetCell{V: "42"}, rawFormatContext, "42", false},
		{"boolean true", generalWorksheetCell{T: "b", V: "1"}, rawFormatContext, "TRUE", false},
		{"boolean true (xsd lexical form)", generalWorksheetCell{T: "b", V: "true"}, rawFormatContext, "TRUE", false},
		{"boolean false", generalWorksheetCell{T: "b", V: "0"}, rawFormatContext, "FALSE", false},
		{"formula with cached value", generalWorksheetCell{T: "str", V: "3", F: "1+2"}, rawFormatContext, "3", false},
		{"formula without cached value", generalWorksheetCell{F: "1+2"}, rawFormatContext, "=1+2", false},
		{"error cell", generalWorksheetCell{T: "e", V: "#DIV/0!"}, rawFormatContext, "#DIV/0!", false},
		{"empty cell", generalWorksheetCell{}, rawFormatContext, "", false},
		{"shared string rejected", generalWorksheetCell{R: "A1", T: "s", V: "0"}, rawFormatContext, "", true},
		{"unsupported type rejected", generalWorksheetCell{R: "A1", T: "z"}, rawFormatContext, "", true},
		{"number with default style stays raw", generalWorksheetCell{T: "n", V: "1234", S: "0"}, formatted, "1234", false},
		{"number formatted per style", generalWorksheetCell{T: "n", V: "1234", S: "1"}, formatted, "1,234", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := generalCellText(c.cell, c.fc)
			if c.wantErr {
				if err == nil {
					t.Fatalf("generalCellText(%+v) = %q, want error", c.cell, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("generalCellText(%+v) unexpected error: %v", c.cell, err)
			}
			if got != c.want {
				t.Errorf("generalCellText(%+v) = %q, want %q", c.cell, got, c.want)
			}
		})
	}
}

func TestGeneralCellText_InlineStringRuns(t *testing.T) {
	// Rich-text inline strings carry runs (<is><r><t>..</t></r>...)
	// instead of a single <is><t>; their text is the concatenation.
	cell := generalWorksheetCell{T: "inlineStr"}
	cell.Is.R = []struct {
		T string `xml:"t"`
	}{{T: "Hello, "}, {T: "world"}}

	got, err := generalCellText(cell, rawFormatContext)
	if err != nil {
		t.Fatalf("generalCellText: %v", err)
	}
	if want := "Hello, world"; got != want {
		t.Errorf("generalCellText = %q, want %q", got, want)
	}

	// A plain single-<t> inline string still reads as before.
	plain := generalWorksheetCell{T: "inlineStr"}
	plain.Is.T = "plain"

	got, err = generalCellText(plain, rawFormatContext)
	if err != nil {
		t.Fatalf("generalCellText: %v", err)
	}
	if want := "plain"; got != want {
		t.Errorf("generalCellText = %q, want %q", got, want)
	}
}

func TestFallbackSheetName(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"xl/worksheets/sheet1.xml", "Sheet1"},
		{"xl/worksheets/sheet12.xml", "Sheet12"},
	}

	for _, c := range cases {
		if got := fallbackSheetName(c.path); got != c.want {
			t.Errorf("fallbackSheetName(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
