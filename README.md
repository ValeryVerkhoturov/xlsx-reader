# xlsx-reader

A small, dependency-free Go package (stdlib only) that reads *any* `.xlsx` (OOXML spreadsheet) workbook — Excel, LibreOffice, Google Sheets, openpyxl, whatever wrote it — through a forward-only, constant-memory row iterator, without ever buffering the whole workbook in memory or seeking.

This repo has no dependencies — build, test, or otherwise — beyond the Go standard library.

> **Warning**
> Shared strings (`xl/sharedStrings.xml`) are not supported, and this is not a corner case: it's how Excel, LibreOffice, and most other writers store almost all text by default, so **most real-world `.xlsx` files use it**. A cell that references the shared-strings table (`t="s"`) makes row iteration fail with an error naming shared strings specifically, rather than silently returning wrong or empty text. Numbers, booleans, formulas, and inline strings (`t="inlineStr"`) are unaffected — see below for why shared strings specifically can't be supported by a forward-only reader.

```
go get github.com/yurij-lyubskij/xlsx-reader
```

## Usage

```go
in, err := os.Open("workbook.xlsx")
if err != nil {
    log.Fatal(err)
}
defer in.Close()

rd, err := xlsx.OpenReader(in) // pass xlsx.RawCellValue(true) to disable number/date formatting
if err != nil {
    log.Fatal(err)
}

for {
    sheet, err := rd.NextSheet()
    if err != nil {
        log.Fatal(err)
    }
    if sheet == nil {
        break // no more sheets
    }

    rows := sheet.Rows()
    for rows.Next() {
        fmt.Println(sheet.Name, rows.Number(), rows.Columns())
    }
    if err := rows.Err(); err != nil {
        log.Fatal(err)
    }
}
```

The trade for that generality is real, and deliberate:

- **No shared strings** (see the warning above). A shared-strings table can legally appear anywhere in the archive, including after the sheet that references it, which would defeat a single forward pass — there's no way to support it without buffering the whole workbook first, which this package deliberately never does.
- **Forward-only, archive order.** Because the ZIP central directory is never consulted, sheets come out of `NextSheet` in the order they physically appear in the archive, not in caller-chosen order, and a `Sheet` is only valid until the next `NextSheet` call — there's no going back to re-read one.
- **Basic number/date formatting, not a general format-code engine.** By default (`RawCellValue(false)`, the zero value — matching [excelize](https://github.com/qax-os/excelize)'s option of the same name and default), a numeric cell whose style resolves to one of a fixed set of built-in number formats (plain/grouped numbers, percentages, the standard date/time formats) or a custom format code that looks date/time-like (e.g. a custom `"yyyy-mm-dd"`) is formatted accordingly; everything else — the default General format, an unrecognized custom code, currency/conditional/color formats, or `xl/styles.xml` simply not having been read yet by the time a sheet is reached — always returns the cell's raw stored text, same as before this option existed. Pass `RawCellValue(true)` to disable formatting entirely and always get raw text.
- **Zip64, with one narrow gap.** Supported whenever the local header itself signals Zip64 (a sentinel size and/or a Zip64 extra field record) — the case Excel, LibreOffice, and any writer targeting a seekable destination use. The one exception: a *streamed* entry (unknown size deferred to a trailing data descriptor) that turns out to need Zip64 without ever signaling that in its local header — notably something Go's own `archive/zip.Writer` can itself produce when writing to a non-seekable `io.Writer` — can't be detected by a forward-only reader, and can silently truncate the rest of the archive rather than error. This requires an unusual combination of circumstances (a non-seekable destination, an entry actually exceeding 4GB, and more archive content after it), documented in `zipWalker.next`'s doc comment rather than hidden.
- **Sheet naming is best-effort.** Names and workbook order come from the workbook part (`xl/workbook.xml`) and its relationships file, which by near-universal convention appear before any worksheet part — but nothing mandates it. If a worksheet is encountered first, its `Sheet` falls back to a name/index derived from the archive itself (e.g. `xl/worksheets/sheet3.xml` → `"Sheet3"`, numbered by the order worksheets appeared).

## Design notes

- **Streaming**: nothing is buffered beyond one row at a time and one small in-memory copy of the workbook/styles metadata (bounded by sheet/style count, not data), so memory use stays roughly constant regardless of workbook size.
- **No random access**: the ZIP central directory is never read, and shared strings are rejected lazily per-cell rather than resolved via an upfront scan for `xl/sharedStrings.xml` — both are what make single-pass reading from a plain `io.Reader` possible at all.

## Testing

```
go test ./...
```

Regenerate the real-world (LibreOffice-produced) `.xlsx` fixtures under `testdata/` used by the tests: `sh testdata/generate.sh` (requires `soffice` on `PATH`; only needed if those fixtures need to change — they're checked in).

One test, `TestReader_FallbackSheetNaming`, exercises the fallback (workbook metadata not yet available) sheet-naming path against a hand-built workbook (built entirely with the stdlib `archive/zip`) whose worksheet parts precede `xl/workbook.xml` — the same unconventional-but-legal ordering a writer that can't seek back to finish that part until every sheet has been streamed out would produce.

## License

MIT — see [LICENSE](LICENSE).
