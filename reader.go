// Package xlsx reads any .xlsx (OOXML spreadsheet) workbook — Excel,
// LibreOffice, Google Sheets, openpyxl, whatever wrote it — through a
// forward-only, constant-memory row iterator, without ever buffering the
// whole workbook in memory or seeking. Start with OpenReader.
//
// WARNING: shared strings (xl/sharedStrings.xml) are not supported. This
// matters because it's how Excel, LibreOffice, and most other writers
// store almost all text by default — so most real-world .xlsx files will
// hit this. A cell that references the shared-strings table (t="s")
// makes row iteration fail with an error naming shared strings
// specifically, rather than silently returning wrong or empty text.
// Numbers, booleans, formulas, and inline strings (t="inlineStr") are
// unaffected. See OpenReader's doc comment for why.
package xlsx

import (
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/yurij-lyubskij/xlsx-reader/zipstream"
)

// Reader provides forward-only, constant-memory access to any .xlsx
// workbook. See OpenReader.
type Reader struct {
	zw  *zipstream.Walker
	cur *zipstream.Entry // the entry backing the sheet currently being iterated, if any

	workbookPath string            // "" until resolved from _rels/.rels; defaultWorkbookPath is assumed until then
	wbSheets     []wbSheetRef      // nil until the workbook part has been parsed
	relsMap      map[string]string // rID -> part path; nil until the workbook part's rels have been parsed
	targetIndex  map[string]resolvedSheet
	date1904     bool // from the workbook part's <workbookPr date1904="1"/>

	numFmts      map[int]string // custom numFmtId -> formatCode; nil until xl/styles.xml has been parsed
	cellXfs      []int          // style index -> numFmtId
	stylesReady  bool
	rawCellValue bool

	sheetsSeen int // count of worksheet entries encountered so far, for fallback numbering

	done bool
}

type resolvedSheet struct {
	name  string
	index int
}

// ReaderOption configures OpenReader. See RawCellValue.
type ReaderOption func(*readerOptions)

type readerOptions struct {
	raw           bool
	zip64Mode     zipstream.Zip64Mode
	decompressors map[uint16]zipstream.Decompressor
}

// RawCellValue controls whether numeric cells are returned as their raw
// stored text (raw=true) or formatted according to the cell's number
// format when it's one this package recognizes (raw=false, the
// default) — mirroring excelize's option of the same name and default.
//
// Formatting here is deliberately basic, not a general Excel
// format-code engine: only a fixed set of built-in numFmtIds (plain and
// grouped numbers, percentages, and the standard date/time formats) and
// custom format codes that look date/time-like are recognized. Anything
// else — including the default General format, an unrecognized custom
// code, or styles.xml not being available yet — always returns raw
// text, regardless of this option. See formatContext and formatBuiltin
// in numfmt.go for the exact scope.
func RawCellValue(raw bool) ReaderOption {
	return func(o *readerOptions) {
		o.raw = raw
	}
}

// Zip64Mode controls how OpenReader resolves the one genuinely ambiguous
// Zip64 scenario a forward-only ZIP reader can encounter — see
// zipstream.Zip64Mode for the full explanation and WithZip64Mode to set
// it. This is an alias for zipstream.Zip64Mode, so a zipstream.Zip64Mode
// value works here directly and vice versa.
type Zip64Mode = zipstream.Zip64Mode

// Zip64Auto, Zip64Force32, and Zip64Force64 are the possible values of
// Zip64Mode; see zipstream.Zip64Mode for what each means.
const (
	Zip64Auto    = zipstream.Zip64Auto
	Zip64Force32 = zipstream.Zip64Force32
	Zip64Force64 = zipstream.Zip64Force64
)

// WithZip64Mode overrides how OpenReader's underlying ZIP walker
// resolves the one genuinely ambiguous Zip64 case it can encounter: a
// streamed worksheet part whose local header signals Zip64 neither via
// a sentinel size nor a Zip64 extra-field record. The default,
// Zip64Auto, matches OpenReader's historical behavior (assume narrow,
// 32-bit framing) exactly; pass Zip64Force64 if the source archive is
// known or suspected to come from a writer (such as Go's own
// archive/zip.Writer, streaming to a non-seekable destination) that can
// produce this ambiguity.
//
// This applies to the whole workbook, not a single sheet: Zip64Force64
// is only appropriate when every ambiguous worksheet part in the
// archive genuinely needs wide framing, since a forward-only reader has
// no way to tell which specific part needs it. See zipstream.Zip64Mode
// for the full explanation.
func WithZip64Mode(mode Zip64Mode) ReaderOption {
	return func(o *readerOptions) {
		o.zip64Mode = mode
	}
}

// Decompressor is an alias for zipstream.Decompressor; see it and
// WithDecompressor.
type Decompressor = zipstream.Decompressor

// WithDecompressor registers dcomp as the decompressor OpenReader's
// underlying ZIP walker uses for method. Only needed when a workbook's
// own parts use a compression method other than Store or Deflate —
// exceedingly rare for real .xlsx files (Excel, LibreOffice, and every
// other writer this package has been tested against use only those two)
// but not forbidden by the ZIP or OOXML formats. See
// zipstream.Decompressor and zipstream.WithDecompressor for the full
// explanation, including the self-terminating-read requirement a
// Decompressor must meet for a streamed worksheet part.
func WithDecompressor(method uint16, dcomp Decompressor) ReaderOption {
	return func(o *readerOptions) {
		if o.decompressors == nil {
			o.decompressors = make(map[uint16]Decompressor)
		}

		o.decompressors[method] = dcomp
	}
}

// OpenReader wraps r for streaming, constant-memory reading of any
// .xlsx workbook. Archive entries are read strictly in the order they
// appear — the ZIP central directory is never consulted, so r need only
// be an io.Reader (e.g. an HTTP response body, not necessarily a file)
// — which means sheets come out of NextSheet in archive order, not
// caller-chosen order, and a sheet can't be revisited once passed.
//
// IMPORTANT: shared strings (xl/sharedStrings.xml) are not supported,
// and this is not a corner case — it's how Excel, LibreOffice, and most
// other writers store almost all text by default, so most real-world
// .xlsx files use it. A cell that actually references the shared-strings
// table (t="s") is rejected the moment it's read, not any earlier — a
// shared-strings table can legally appear anywhere in the archive,
// including after the sheet that references it, which would defeat a
// single forward pass, so there's no way to support it without buffering
// the whole workbook first. Numbers, booleans, formulas, and inline
// strings (t="inlineStr") all still read fine in such a file; only the
// shared-string cells error, with an error that names shared strings
// specifically so the cause is never ambiguous.
//
// Other deliberate restrictions keep this tractable without random
// access to the archive:
//
//   - number/date formatting (see RawCellValue) is basic and best
//     effort: only a fixed set of built-in and date-like custom number
//     formats are recognized, and it depends on xl/styles.xml having
//     been seen already — see the archive-ordering caveat below, which
//     applies to this exactly as it does to sheet naming.
//
// Sheet naming (Sheet.Name/Sheet.Index) depends on the workbook part
// (conventionally xl/workbook.xml) and its relationships file having
// already been parsed by the time a worksheet part is reached; number
// and date formatting depend on xl/styles.xml the same way. Nothing in the
// ZIP or OPC formats mandates writing that metadata before any worksheet
// part, and — despite that being the natural assumption — it is not a
// rare exception in practice: LibreOffice and OnlyOffice reliably write
// xl/workbook.xml and xl/styles.xml first (verified against real output
// from both), and Excel is conventionally assumed to as well, but
// xuri/excelize, openpyxl, and WPS Office have all been observed doing
// the exact opposite — every worksheet part before that metadata — as a
// matter of course, not as a corner case; this has nothing to do with
// being unable to seek back to patch an already-written part (all three
// of those writers buffer the whole archive before writing it out) and
// everything to do with the order each one simply chooses to emit parts
// in, which OOXML leaves entirely up to the writer. When a worksheet is
// reached before that metadata is available, its Sheet falls back to a
// name/index derived from the archive itself (its part path and the
// order worksheets appear in) rather than failing outright, and every
// numeric/date cell on it stays raw regardless of RawCellValue — both
// silently, with no error. Don't assume Sheet.Name, Sheet.Index, or a
// RawCellValue(false) cell's formatting are meaningful for a workbook
// from an unverified writer without checking; RawCellValue(true) plus
// formatting the raw value yourself sidesteps the formatting half of
// this entirely, since it never depends on xl/styles.xml having been
// seen.
//
// Archive entries are read via the zipstream package's forward-only
// Walker (github.com/yurij-lyubskij/xlsx-reader/zipstream), which OpenReader
// consumes as a public, standalone API in its own right — it has nothing
// xlsx-specific about it and can be used to stream-read any ZIP archive.
//
// Zip64 archives are supported when the local header itself signals
// Zip64 (the common case: Excel, LibreOffice, and any writer with a
// seekable destination). One narrow case doesn't: a streamed entry
// whose writer never signaled Zip64 in the local header at all can
// silently truncate the rest of the archive rather than error — see
// zipstream.Walker.Next's doc comment for why this is undetectable
// without buffering ahead, and how rare a combination of circumstances
// it requires. Use WithZip64Mode(Zip64Force64) to opt into resolving
// that ambiguity correctly when the source archive is known or
// suspected to hit it.
//
// Only Store and Deflate compression are understood without further
// help — the only two methods any writer this package has been tested
// against ever produces. Use WithDecompressor to add support for
// another method a workbook's parts might use.
func OpenReader(r io.Reader, opts ...ReaderOption) (*Reader, error) {
	var o readerOptions
	for _, opt := range opts {
		opt(&o)
	}

	zwOpts := []zipstream.Option{zipstream.WithZip64Mode(o.zip64Mode)}
	for method, dcomp := range o.decompressors {
		zwOpts = append(zwOpts, zipstream.WithDecompressor(method, dcomp))
	}

	zw := zipstream.New(r, zwOpts...)

	return &Reader{zw: zw, rawCellValue: o.raw}, nil
}

// Sheet describes one worksheet as returned by Reader.NextSheet. Its
// Rows method must be called (and its iterator either drained or
// abandoned) before the next call to NextSheet — a Sheet is only valid
// until then.
type Sheet struct {
	Name  string
	Index int // 1-based; from the workbook part's <sheets> order when known, otherwise the order worksheets appear in the archive

	r      *Reader
	entry  *zipstream.Entry
	fmtCtx *formatContext
	rows   *RowIterator // cached, so repeated Rows calls share one in-progress iterator
}

// Rows returns a row iterator over sheet's data. Calling it more than
// once for the same Sheet returns the same, already-in-progress
// iterator. Calling it on a Sheet that is no longer the Reader's current
// sheet (because NextSheet has since been called) returns an iterator
// whose first Next call returns false.
func (s *Sheet) Rows() *RowIterator {
	if s.r.cur != s.entry {
		return &RowIterator{stale: true}
	}

	if s.rows == nil {
		s.rows = newRowIterator(s.entry, s.fmtCtx)
	}

	return s.rows
}

// NextSheet advances to the next worksheet in archive order, discarding
// any rows left unread on the previously-returned sheet. It returns
// (nil, nil) once every sheet in the archive has been consumed.
func (r *Reader) NextSheet() (*Sheet, error) {
	if r.done {
		return nil, nil
	}

	if r.cur != nil {
		if err := r.cur.Finish(); err != nil {
			return nil, err
		}

		r.cur = nil
	}

	for {
		name, entry, err := r.zw.Next()
		if err != nil {
			return nil, err
		}

		if entry == nil {
			r.done = true
			return nil, nil
		}

		switch {
		case name == "_rels/.rels":
			wbPath, err := parseRootRels(entry)
			if err != nil {
				return nil, err
			}

			if err := entry.Finish(); err != nil {
				return nil, err
			}

			if wbPath != "" {
				r.workbookPath = wbPath
			}

		case name == r.expectedWorkbookPath():
			sheets, date1904, err := parseWorkbook(entry)
			if err != nil {
				return nil, err
			}

			if err := entry.Finish(); err != nil {
				return nil, err
			}

			r.wbSheets = sheets
			r.date1904 = date1904
			r.targetIndex = nil

		case name == relsPathFor(r.expectedWorkbookPath()):
			relsMap, err := parseWorkbookRels(entry, path.Dir(r.expectedWorkbookPath()))
			if err != nil {
				return nil, err
			}

			if err := entry.Finish(); err != nil {
				return nil, err
			}

			r.relsMap = relsMap
			r.targetIndex = nil

		case name == defaultStylesPath:
			numFmts, cellXfs, err := parseStyles(entry)
			if err != nil {
				return nil, err
			}

			if err := entry.Finish(); err != nil {
				return nil, err
			}

			r.numFmts = numFmts
			r.cellXfs = cellXfs
			r.stylesReady = true

		case looksLikeWorksheetPart(name):
			r.sheetsSeen++
			r.cur = entry

			sheet := r.resolveSheet(name)
			sheet.r = r
			sheet.entry = entry
			sheet.fmtCtx = &formatContext{
				raw:      r.rawCellValue,
				ready:    r.stylesReady,
				cellXfs:  r.cellXfs,
				numFmts:  r.numFmts,
				date1904: r.date1904,
			}

			return sheet, nil

		default:
			if err := entry.Finish(); err != nil {
				return nil, err
			}
		}
	}
}

func (r *Reader) expectedWorkbookPath() string {
	if r.workbookPath != "" {
		return r.workbookPath
	}

	return defaultWorkbookPath
}

// resolveSheet resolves partPath to its declared name and workbook
// order, if the workbook part and its rels have both been parsed by
// now; otherwise (or if partPath isn't found in that metadata) it falls
// back to a name derived from the part's own filename and the order
// worksheets have appeared in the archive.
func (r *Reader) resolveSheet(partPath string) *Sheet {
	if r.wbSheets != nil && r.relsMap != nil {
		if r.targetIndex == nil {
			r.targetIndex = make(map[string]resolvedSheet, len(r.wbSheets))

			for i, s := range r.wbSheets {
				target, ok := r.relsMap[s.rID]
				if !ok {
					continue
				}

				r.targetIndex[target] = resolvedSheet{name: s.name, index: i + 1}
			}
		}

		if rs, ok := r.targetIndex[partPath]; ok {
			return &Sheet{Name: rs.name, Index: rs.index}
		}
	}

	return &Sheet{Name: fallbackSheetName(partPath), Index: r.sheetsSeen}
}

// looksLikeWorksheetPart reports whether name is a direct child of
// xl/worksheets/ named *.xml — the directory and naming convention
// every OOXML writer this package has been tested against uses for
// worksheet parts, and the only signal this reader needs to detect one:
// unlike naming, it can't depend on the workbook's rels having been
// parsed yet.
func looksLikeWorksheetPart(name string) bool {
	const prefix = "xl/worksheets/"

	if !strings.HasPrefix(name, prefix) {
		return false
	}

	rest := name[len(prefix):]

	return rest != "" && !strings.Contains(rest, "/") && strings.HasSuffix(rest, ".xml")
}

// fallbackSheetName derives a display name from a worksheet part's own
// path, e.g. "xl/worksheets/sheet3.xml" -> "Sheet3", used when the
// workbook part's metadata isn't available (or doesn't mention this
// part) by the time it's read.
func fallbackSheetName(partPath string) string {
	base := path.Base(partPath)
	base = strings.TrimSuffix(base, path.Ext(base))

	if base == "" {
		return partPath
	}

	return strings.ToUpper(base[:1]) + base[1:]
}

// RowIterator iterates one worksheet's rows in document order. Obtain
// one from Sheet.Rows.
type RowIterator struct {
	dec        *xml.Decoder
	entry      *zipstream.Entry
	fc         *formatContext
	lastNumber int
	number     int
	columns    []string
	err        error
	done       bool
	stale      bool
}

func newRowIterator(entry *zipstream.Entry, fc *formatContext) *RowIterator {
	return &RowIterator{
		dec:   xml.NewDecoder(entry),
		entry: entry,
		fc:    fc,
	}
}

// Next advances to the next <row> physically present in the sheet's
// data (a fully blank row, which real writers omit from the XML
// entirely rather than writing an empty element, is not synthesized —
// check Number if gaps matter to the caller). It returns false once the
// sheet is exhausted or an error occurs; call Err afterward to tell the
// two apart.
func (it *RowIterator) Next() bool {
	if it.stale || it.done || it.err != nil {
		return false
	}

	for {
		tok, err := it.dec.Token()

		if err == io.EOF {
			it.done = true
			return false
		}

		if err != nil {
			it.err = fmt.Errorf("xlsx: parsing worksheet: %w", err)
			return false
		}

		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "row" {
			continue
		}

		var row struct {
			R     string                 `xml:"r,attr"`
			Cells []generalWorksheetCell `xml:"c"`
		}

		if err := it.dec.DecodeElement(&row, &start); err != nil {
			it.err = fmt.Errorf("xlsx: parsing row: %w", err)
			return false
		}

		number, columns, err := buildRow(row.R, row.Cells, it.fc)
		if err != nil {
			it.err = err
			return false
		}

		if number == 0 {
			number = it.lastNumber + 1
		}

		it.lastNumber = number
		it.number = number
		it.columns = columns

		return true
	}
}

// Number is the current row's 1-based row number, from its r attribute
// when present, otherwise one past the previous row's number.
func (it *RowIterator) Number() int { return it.number }

// Columns returns the current row's cell values as raw text, gap-filled
// with "" for any column the XML omitted — sparse rows are legal OOXML,
// and real-world workbooks produce them.
func (it *RowIterator) Columns() []string { return it.columns }

// Err returns the error, if any, that caused the last Next call to
// return false. It returns nil if the sheet was simply exhausted.
func (it *RowIterator) Err() error { return it.err }

// generalWorksheetCell is one <c> element as any general-purpose OOXML
// writer might produce it — a broader shape than a reader that only ever
// has to decode its own narrower output would need.
type generalWorksheetCell struct {
	R  string `xml:"r,attr"`
	S  string `xml:"s,attr"`
	T  string `xml:"t,attr"`
	V  string `xml:"v"`
	F  string `xml:"f"`
	Is struct {
		T string `xml:"t"`
		R []struct {
			T string `xml:"t"`
		} `xml:"r"`
	} `xml:"is"`
}

// buildRow converts a row's cells to gap-filled column text, resolving
// each cell's position from its r attribute (e.g. "C5") rather than
// assuming one <c> per column in order, since OOXML allows (and real
// writers produce) sparse rows that skip empty cells entirely.
func buildRow(rAttr string, cells []generalWorksheetCell, fc *formatContext) (int, []string, error) {
	number := 0

	if rAttr != "" {
		n, err := strconv.Atoi(rAttr)
		if err != nil {
			return 0, nil, fmt.Errorf("xlsx: row has invalid r attribute %q: %w", rAttr, err)
		}

		number = n
	}

	columns := make([]string, 0, len(cells))
	nextCol := 1

	for _, c := range cells {
		colIdx := nextCol

		if c.R != "" {
			idx, err := parseCellColumn(c.R)
			if err != nil {
				return 0, nil, fmt.Errorf("xlsx: parsing row: %w", err)
			}

			colIdx = idx
		}

		for len(columns) < colIdx-1 {
			columns = append(columns, "")
		}

		text, err := generalCellText(c, fc)
		if err != nil {
			return 0, nil, err
		}

		if len(columns) < colIdx {
			columns = append(columns, text)
		} else {
			columns[colIdx-1] = text
		}

		nextCol = colIdx + 1
	}

	return number, columns, nil
}

// parseCellColumn parses the column-letter prefix of a cell reference
// like "AC12" into its 1-based column index. Real writers always
// uppercase, but lowercase letters are accepted too since the grammar is
// otherwise identical.
func parseCellColumn(ref string) (int, error) {
	i := 0
	for i < len(ref) && (ref[i] >= 'A' && ref[i] <= 'Z' || ref[i] >= 'a' && ref[i] <= 'z') {
		i++
	}

	if i == 0 {
		return 0, fmt.Errorf("xlsx: cell reference %q has no column letters", ref)
	}

	col := 0
	for _, ch := range ref[:i] {
		if ch >= 'a' {
			ch -= 'a' - 'A'
		}

		col = col*26 + int(ch-'A'+1)
	}

	return col, nil
}

// generalCellText converts one cell to text. Numeric cells get
// number/date formatting applied per fc (see formatContext) rather than
// always staying raw. Shared strings (t="s") are rejected here, lazily,
// rather than when xl/sharedStrings.xml itself is seen as an archive
// entry, so a workbook with an unused shared-strings part still reads
// fine.
func generalCellText(c generalWorksheetCell, fc *formatContext) (string, error) {
	switch c.T {
	case "s":
		return "", fmt.Errorf("cell %s: shared strings (t=\"s\") are not supported by this reader", c.R)
	case "inlineStr":
		// Rich-text cells carry runs (<is><r><t>..</t></r>...) instead of
		// a single <is><t>; concatenate the run text.
		if len(c.Is.R) > 0 {
			var b strings.Builder
			for _, r := range c.Is.R {
				b.WriteString(r.T)
			}

			return b.String(), nil
		}

		return c.Is.T, nil
	case "b":
		if c.V != "" {
			if c.V == "1" || strings.EqualFold(c.V, "true") {
				return "TRUE", nil
			}

			return "FALSE", nil
		}
	case "", "n":
		if c.V != "" {
			return fc.formatCellValue(c.V, c.S), nil
		}
	case "str", "e":
		if c.V != "" {
			return c.V, nil
		}
	default:
		return "", fmt.Errorf("cell %s: unsupported cell type %q", c.R, c.T)
	}

	if c.F != "" {
		return "=" + c.F, nil
	}

	return "", nil
}
