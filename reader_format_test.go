package xlsx

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"testing"
)

// buildSyntheticXLSX hand-builds a minimal, non-self-produced-shaped
// .xlsx with full control over styles.xml's cellXfs assignments, needed
// to exercise specific numFmtIds deterministically -- something
// LibreOffice's CSV importer doesn't reliably let a test dictate.
// includeStyles controls whether xl/styles.xml is written at all, to
// test the "styles metadata never arrives" no-op path.
func buildSyntheticXLSX(t *testing.T, date1904, includeStyles bool, stylesXML, sheetXML string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	write := func(name, content string) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	write("_rels/.rels", `<?xml version="1.0" encoding="UTF-8"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`)

	var date1904Attr string
	if date1904 {
		date1904Attr = ` date1904="1"`
	}
	write("xl/workbook.xml", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<workbookPr%s/>
<sheets><sheet name="Data" sheetId="1" r:id="rId1"/></sheets>
</workbook>`, date1904Attr))

	write("xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
</Relationships>`)

	if includeStyles {
		write("xl/styles.xml", stylesXML)
	}

	write("xl/worksheets/sheet1.xml", sheetXML)

	if err := zw.Close(); err != nil {
		t.Fatalf("closing synthetic archive: %v", err)
	}

	return buf.Bytes()
}

// basicStylesXML defines cellXfs index -> numFmtId: 0=General,
// 1=built-in 3 ("#,##0"), 2=built-in 14 (date), 3=built-in 9 ("0%"),
// 4=custom 164 ("yyyy-mm-dd").
const basicStylesXML = `<?xml version="1.0" encoding="UTF-8"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<numFmts count="1"><numFmt numFmtId="164" formatCode="yyyy-mm-dd"/></numFmts>
<cellXfs count="5">
<xf numFmtId="0"/>
<xf numFmtId="3"/>
<xf numFmtId="14"/>
<xf numFmtId="9"/>
<xf numFmtId="164"/>
</cellXfs>
</styleSheet>`

func oneRowSheetXML(cells string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<sheetData><row r="1">%s</row></sheetData>
</worksheet>`, cells)
}

func firstRowColumns(t *testing.T, data []byte, opts ...ReaderOption) []string {
	t.Helper()

	rd, err := OpenReader(bytes.NewReader(data), opts...)
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

	rows := sheet.Rows()
	if !rows.Next() {
		t.Fatalf("Rows().Next() = false, err = %v", rows.Err())
	}

	return append([]string(nil), rows.Columns()...)
}

func TestReader_RawCellValue_DisablesFormatting(t *testing.T) {
	data := buildSyntheticXLSX(t, false, true, basicStylesXML,
		oneRowSheetXML(`<c r="A1" s="1"><v>1234</v></c>`))

	if got := firstRowColumns(t, data); !equalStrings(got, []string{"1,234"}) {
		t.Errorf("default (formatted): got %v, want [1,234]", got)
	}

	if got := firstRowColumns(t, data, RawCellValue(false)); !equalStrings(got, []string{"1,234"}) {
		t.Errorf("RawCellValue(false): got %v, want [1,234]", got)
	}

	if got := firstRowColumns(t, data, RawCellValue(true)); !equalStrings(got, []string{"1234"}) {
		t.Errorf("RawCellValue(true): got %v, want [1234]", got)
	}
}

func TestReader_FormatsBuiltinNumFmts(t *testing.T) {
	data := buildSyntheticXLSX(t, false, true, basicStylesXML, oneRowSheetXML(
		`<c r="A1" s="0"><v>42</v></c>`+ // General: unaffected
			`<c r="B1" s="1"><v>1234567</v></c>`+ // #,##0
			`<c r="C1" s="2"><v>45306</v></c>`+ // date
			`<c r="D1" s="3"><v>0.5</v></c>`, // 0%
	))

	want := []string{"42", "1,234,567", "01-15-24", "50%"}
	if got := firstRowColumns(t, data); !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestReader_FormatsCustomDateNumFmt(t *testing.T) {
	data := buildSyntheticXLSX(t, false, true, basicStylesXML,
		oneRowSheetXML(`<c r="A1" s="4"><v>45306</v></c>`))

	if got := firstRowColumns(t, data); !equalStrings(got, []string{"2024-01-15"}) {
		t.Errorf("got %v, want [2024-01-15]", got)
	}
}

func TestReader_Date1904(t *testing.T) {
	sheetXML := oneRowSheetXML(`<c r="A1" s="4"><v>0</v></c>`)

	data1900 := buildSyntheticXLSX(t, false, true, basicStylesXML, sheetXML)
	data1904 := buildSyntheticXLSX(t, true, true, basicStylesXML, sheetXML)

	got1900 := firstRowColumns(t, data1900)
	got1904 := firstRowColumns(t, data1904)

	if got1900[0] == got1904[0] {
		t.Fatalf("expected different output for date1900 vs date1904, both got %q", got1900[0])
	}
	if want := "1904-01-01"; got1904[0] != want {
		t.Errorf("date1904 serial 0: got %q, want %q", got1904[0], want)
	}
}

func TestReader_StylesUnavailable_NoOps(t *testing.T) {
	data := buildSyntheticXLSX(t, false, false, "", // includeStyles=false: xl/styles.xml never appears
		oneRowSheetXML(`<c r="A1" s="1"><v>1234567</v></c>`))

	if got := firstRowColumns(t, data); !equalStrings(got, []string{"1234567"}) {
		t.Errorf("got %v, want [1234567] (raw, since styles.xml never arrived)", got)
	}
}

func TestReader_LibreOfficeDates(t *testing.T) {
	f, err := os.Open("testdata/libreoffice_dates.xlsx")
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
	if sheet.Name != "dates" {
		t.Errorf("Name = %q, want %q", sheet.Name, "dates")
	}

	want := [][]string{{"2024-01-15"}, {"2023-12-25"}}
	got := collectRows(t, sheet)
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
	}
	for i, row := range got {
		if !equalStrings(row, want[i]) {
			t.Errorf("row %d: got %v, want %v", i+1, row, want[i])
		}
	}
}
