package xlsx

import (
	"strings"
	"testing"
)

func TestParseStyles(t *testing.T) {
	xmlText := `<?xml version="1.0" encoding="UTF-8"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
	<numFmts count="2">
		<numFmt numFmtId="164" formatCode="yyyy-mm-dd"/>
		<numFmt numFmtId="165" formatCode="@"/>
	</numFmts>
	<cellXfs count="4">
		<xf numFmtId="0"/>
		<xf numFmtId="3"/>
		<xf/>
		<xf numFmtId="164"/>
	</cellXfs>
</styleSheet>`

	numFmts, cellXfs, err := parseStyles(strings.NewReader(xmlText))
	if err != nil {
		t.Fatalf("parseStyles: %v", err)
	}

	wantNumFmts := map[int]string{164: "yyyy-mm-dd", 165: "@"}
	if len(numFmts) != len(wantNumFmts) {
		t.Fatalf("numFmts = %v, want %v", numFmts, wantNumFmts)
	}
	for id, code := range wantNumFmts {
		if numFmts[id] != code {
			t.Errorf("numFmts[%d] = %q, want %q", id, numFmts[id], code)
		}
	}

	wantCellXfs := []int{0, 3, 0, 164} // the 3rd <xf> has no numFmtId attribute -> defaults to 0
	if !equalInts(cellXfs, wantCellXfs) {
		t.Errorf("cellXfs = %v, want %v", cellXfs, wantCellXfs)
	}
}

func equalInts(a, b []int) bool {
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
