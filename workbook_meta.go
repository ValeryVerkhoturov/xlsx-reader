package xlsx

import (
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strings"
)

// defaultWorkbookPath is the part path OpenReader assumes for the
// workbook part until (or unless) _rels/.rels resolves the real one —
// true for every real-world .xlsx writer this package has been tested
// against.
const defaultWorkbookPath = "xl/workbook.xml"

// wbSheetRef is one <sheet> entry from the workbook part's <sheets>
// list: its display name and the relationship id that resolves to its
// worksheet part's path.
type wbSheetRef struct {
	name string
	rID  string
}

type relationshipsXML struct {
	Relationships []struct {
		ID     string `xml:"Id,attr"`
		Type   string `xml:"Type,attr"`
		Target string `xml:"Target,attr"`
	} `xml:"Relationship"`
}

// parseRootRels parses _rels/.rels and returns the target of its
// officeDocument relationship — the workbook part's path — or "" if none
// is present.
func parseRootRels(r io.Reader) (workbookPath string, err error) {
	var doc relationshipsXML

	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return "", fmt.Errorf("xlsx: parsing _rels/.rels: %w", err)
	}

	for _, rel := range doc.Relationships {
		if strings.HasSuffix(rel.Type, "/officeDocument") {
			return strings.TrimPrefix(rel.Target, "/"), nil
		}
	}

	return "", nil
}

type workbookPartXML struct {
	WorkbookPr struct {
		Date1904 string `xml:"date1904,attr"`
	} `xml:"workbookPr"`
	Sheets []struct {
		Name string `xml:"name,attr"`
		RID  string `xml:"id,attr"`
	} `xml:"sheets>sheet"`
}

// parseWorkbook parses the workbook part, returning its sheets in
// document order (the order tabs appear in the workbook) and whether
// the workbook uses the 1904 date system (<workbookPr date1904="1"/>;
// "true" is also accepted per the XML boolean attribute grammar, though
// every real writer this package has seen uses "1"). Everything else
// defaults to the far more common 1900 system.
func parseWorkbook(r io.Reader) ([]wbSheetRef, bool, error) {
	var doc workbookPartXML

	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, false, fmt.Errorf("xlsx: parsing workbook part: %w", err)
	}

	refs := make([]wbSheetRef, len(doc.Sheets))
	for i, s := range doc.Sheets {
		refs[i] = wbSheetRef{name: s.Name, rID: s.RID}
	}

	date1904 := doc.WorkbookPr.Date1904 == "1" || strings.EqualFold(doc.WorkbookPr.Date1904, "true")

	return refs, date1904, nil
}

// parseWorkbookRels parses the workbook part's .rels file, returning a
// map from relationship id to the referenced part's path, resolved
// relative to dir (the workbook part's own directory, e.g. "xl").
func parseWorkbookRels(r io.Reader, dir string) (map[string]string, error) {
	var doc relationshipsXML

	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("xlsx: parsing workbook part rels: %w", err)
	}

	out := make(map[string]string, len(doc.Relationships))
	for _, rel := range doc.Relationships {
		out[rel.ID] = resolvePartPath(dir, rel.Target)
	}

	return out, nil
}

func resolvePartPath(dir, target string) string {
	if strings.HasPrefix(target, "/") {
		return target[1:]
	}

	return path.Join(dir, target)
}

// relsPathFor returns the OPC-conventional .rels part path for
// partPath, e.g. "xl/workbook.xml" -> "xl/_rels/workbook.xml.rels".
func relsPathFor(partPath string) string {
	dir := path.Dir(partPath)
	base := path.Base(partPath)

	return path.Join(dir, "_rels", base+".rels")
}
