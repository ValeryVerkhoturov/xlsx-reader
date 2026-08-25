package xlsx

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// defaultStylesPath is the part path OpenReader recognizes for the
// styles part. Unlike the workbook part, this is never resolved via
// rels -- every writer this package has been tested against uses this
// exact conventional path, and a style/format lookup is inherently
// best-effort (see formatContext), so there's no need for the
// indirection defaultWorkbookPath's fallback affords.
const defaultStylesPath = "xl/styles.xml"

type styleSheetXML struct {
	NumFmts struct {
		NumFmt []struct {
			ID   int    `xml:"numFmtId,attr"`
			Code string `xml:"formatCode,attr"`
		} `xml:"numFmt"`
	} `xml:"numFmts"`
	CellXfs struct {
		Xf []struct {
			NumFmtID          int    `xml:"numFmtId,attr"`
			ApplyNumberFormat string `xml:"applyNumberFormat,attr"`
		} `xml:"xf"`
	} `xml:"cellXfs"`
}

// parseStyles parses the styles part, returning its custom number
// formats (numFmtId -> formatCode, for ids >= 164 by convention, though
// nothing here enforces that) and its cellXfs list (style index ->
// numFmtId; an <xf> with no numFmtId attribute defaults to 0/General,
// matching the XML zero value). An <xf applyNumberFormat="0"/> (or
// "false") means its numFmtId should not be applied, so that xf is
// recorded as 0/General instead -- an <xf> with no applyNumberFormat
// attribute at all keeps its numFmtId unchanged, matching every real
// writer this package has been tested against.
func parseStyles(r io.Reader) (numFmts map[int]string, cellXfs []int, err error) {
	var doc styleSheetXML

	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("xlsx: parsing styles part: %w", err)
	}

	numFmts = make(map[int]string, len(doc.NumFmts.NumFmt))
	for _, nf := range doc.NumFmts.NumFmt {
		numFmts[nf.ID] = nf.Code
	}

	cellXfs = make([]int, len(doc.CellXfs.Xf))
	for i, xf := range doc.CellXfs.Xf {
		id := xf.NumFmtID
		if xf.ApplyNumberFormat == "0" || strings.EqualFold(xf.ApplyNumberFormat, "false") {
			id = 0
		}

		cellXfs[i] = id
	}

	return numFmts, cellXfs, nil
}
