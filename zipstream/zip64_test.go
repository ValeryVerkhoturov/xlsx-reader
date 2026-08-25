package zipstream

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"hash/crc32"
	"io"
	"strings"
	"testing"
)

// TestWalker_Zip64SentinelWithoutExtraField checks a header that flags
// Zip64 (a sentinel size) but supplies no Zip64 extra field to resolve
// it against -- malformed, so this must fail clearly rather than
// silently treating the sentinel as a literal 4-gigabyte size.
func TestWalker_Zip64SentinelWithoutExtraField(t *testing.T) {
	raw := buildLocalEntry(t, "part.xml", []byte("data"), Deflate, false, false)
	binary.LittleEndian.PutUint32(raw[18:22], zip32SizeSentinel) // compressed-size Zip64 marker

	w := New(bytes.NewReader(raw))

	_, _, err := w.Next()
	if err == nil {
		t.Fatal("Next() = nil error on an unresolved Zip64 marker, want an error")
	}
	if !strings.Contains(err.Error(), "Zip64") {
		t.Errorf("err = %v, want it to mention Zip64", err)
	}
}

// compressForTest compresses data per method, returning the compressed
// bytes and data's CRC-32 -- shared by the Zip64 tests below, which need
// to know the real compressed size before they can build a matching
// Zip64 extra field record.
func compressForTest(t *testing.T, data []byte, method uint16) ([]byte, uint32) {
	t.Helper()

	var comp bytes.Buffer

	switch method {
	case Deflate:
		fw, err := flate.NewWriter(&comp, flate.DefaultCompression)
		if err != nil {
			t.Fatalf("flate.NewWriter: %v", err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatalf("compressing test data: %v", err)
		}
		if err := fw.Close(); err != nil {
			t.Fatalf("closing flate writer: %v", err)
		}
	case Store:
		comp.Write(data)
	default:
		t.Fatalf("compressForTest: unsupported method %d", method)
	}

	return comp.Bytes(), crc32.ChecksumIEEE(data)
}

// buildZip64Extra builds a Zip64 extended-information extra-field
// record (zip64ExtraID), carrying the uncompressed then compressed
// 64-bit sizes -- the only order a local header's own Zip64 record
// uses -- including only whichever fields include* asks for, matching
// the "only present if the header field was the sentinel" rule.
func buildZip64Extra(uncompressed, compressed uint64, includeUncompressed, includeCompressed bool) []byte {
	var payload []byte

	if includeUncompressed {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uncompressed)
		payload = append(payload, b[:]...)
	}

	if includeCompressed {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], compressed)
		payload = append(payload, b[:]...)
	}

	var record [4]byte
	binary.LittleEndian.PutUint16(record[0:2], zip64ExtraID)
	binary.LittleEndian.PutUint16(record[2:4], uint16(len(payload)))

	return append(record[:], payload...)
}

// assembleLocalEntry hand-builds one local entry's raw bytes with full,
// independent control over: the header's declared 32-bit size fields
// (so a test can set the Zip64 sentinel regardless of the real
// compressed data), its extra field, and -- for a streamed entry --
// whether the trailing data descriptor's size fields are 4 or 8 bytes
// wide, independent of whether the header signaled Zip64 at all. This
// lets a test build a header-signaled-but-tiny entry (e.g. modeling
// Python's zipfile force_zip64=True) that's still correctly read as
// wide even though needsWideDescriptor's true-size fallback alone would
// have said narrow. compressed/crc/dataLen come from compressForTest.
func assembleLocalEntry(name string, compressed []byte, crc uint32, dataLen int, method uint16, streaming bool, headerCompSize, headerUncompSize uint32, extra []byte, wideDescriptor bool) []byte {
	var header [30]byte
	binary.LittleEndian.PutUint32(header[0:4], zipLocalFileHeaderSignature)
	binary.LittleEndian.PutUint16(header[4:6], 45) // version needed to extract: Zip64-aware

	var gpFlag uint16
	if streaming {
		gpFlag |= 0x8
	}
	binary.LittleEndian.PutUint16(header[6:8], gpFlag)
	binary.LittleEndian.PutUint16(header[8:10], method)

	if !streaming {
		binary.LittleEndian.PutUint32(header[14:18], crc)
	}

	binary.LittleEndian.PutUint32(header[18:22], headerCompSize)
	binary.LittleEndian.PutUint32(header[22:26], headerUncompSize)
	binary.LittleEndian.PutUint16(header[26:28], uint16(len(name)))
	binary.LittleEndian.PutUint16(header[28:30], uint16(len(extra)))

	var buf bytes.Buffer
	buf.Write(header[:])
	buf.WriteString(name)
	buf.Write(extra)
	buf.Write(compressed)

	if streaming {
		var sig [4]byte
		binary.LittleEndian.PutUint32(sig[:], zipDataDescriptorSignature)
		buf.Write(sig[:])

		var crcBytes [4]byte
		binary.LittleEndian.PutUint32(crcBytes[:], crc)
		buf.Write(crcBytes[:])

		writeSize := func(v uint64) {
			if wideDescriptor {
				var b [8]byte
				binary.LittleEndian.PutUint64(b[:], v)
				buf.Write(b[:])
			} else {
				var b [4]byte
				binary.LittleEndian.PutUint32(b[:], uint32(v))
				buf.Write(b[:])
			}
		}

		writeSize(uint64(len(compressed)))
		writeSize(uint64(dataLen))
	}

	return buf.Bytes()
}

func readAllAndFinish(t *testing.T, entry *Entry, want []byte) {
	t.Helper()

	got, err := io.ReadAll(entry)
	if err != nil {
		t.Fatalf("reading entry: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	if err := entry.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

// TestWalker_Zip64NonStreaming covers the common, fully unambiguous
// case: a real writer (Excel, LibreOffice, anything targeting a
// seekable destination) knows its sizes up front and signals Zip64 with
// sentinel header fields plus a matching extra field record. The
// payload here is deliberately tiny -- a real >4GB fixture isn't
// practical in a test -- which is itself realistic: some writers (e.g.
// Python's zipfile with force_zip64=True) always use Zip64 regardless
// of actual size.
func TestWalker_Zip64NonStreaming(t *testing.T) {
	data := []byte("zip64-flagged but tiny payload; sizes come from the extra field, not the header")
	compressed, crc := compressForTest(t, data, Deflate)
	extra := buildZip64Extra(uint64(len(data)), uint64(len(compressed)), true, true)

	raw := assembleLocalEntry("part.xml", compressed, crc, len(data), Deflate, false,
		zip32SizeSentinel, zip32SizeSentinel, extra, false)

	w := New(bytes.NewReader(raw))

	name, entry, err := w.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if name != "part.xml" {
		t.Fatalf("name = %q, want %q", name, "part.xml")
	}

	readAllAndFinish(t, entry, data)
}

// TestWalker_Zip64NonStreaming_OnlyOneSizeFlagged checks the "only if
// flagged" consumption rule: when just the compressed size is a
// sentinel, the extra field supplies only that field, and the
// (non-sentinel) uncompressed size in the header itself is left alone.
func TestWalker_Zip64NonStreaming_OnlyOneSizeFlagged(t *testing.T) {
	data := []byte("only the compressed size needs Zip64 resolution here")
	compressed, crc := compressForTest(t, data, Deflate)
	extra := buildZip64Extra(0, uint64(len(compressed)), false, true)

	raw := assembleLocalEntry("part.xml", compressed, crc, len(data), Deflate, false,
		zip32SizeSentinel, uint32(len(data)), extra, false)

	w := New(bytes.NewReader(raw))

	_, entry, err := w.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	readAllAndFinish(t, entry, data)
}

// TestWalker_Zip64Streaming_SignaledBySentinel covers a streamed entry
// that explicitly signals Zip64 (sentinel sizes plus an extra field
// record) in its local header -- unambiguous, so the trailing data
// descriptor is correctly read as 8-byte-wide.
func TestWalker_Zip64Streaming_SignaledBySentinel(t *testing.T) {
	data := []byte("streamed, zip64-sentinel'd, wide trailing data descriptor")
	compressed, crc := compressForTest(t, data, Deflate)
	extra := buildZip64Extra(uint64(len(data)), uint64(len(compressed)), true, true)

	raw := assembleLocalEntry("xl/worksheets/sheet1.xml", compressed, crc, len(data), Deflate, true,
		zip32SizeSentinel, zip32SizeSentinel, extra, true)

	w := New(bytes.NewReader(raw))

	_, entry, err := w.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	readAllAndFinish(t, entry, data)
}

// TestWalker_Zip64Streaming_SignaledByExtraFieldOnly covers a streamed
// entry whose header sizes are the ordinary 0 (not the sentinel) but
// which still carries a Zip64 extra field record -- how a well-behaved
// streaming writer flags Zip64 before it knows the real sizes. Presence
// of the record alone is treated as a reliable signal that the trailing
// descriptor is wide.
func TestWalker_Zip64Streaming_SignaledByExtraFieldOnly(t *testing.T) {
	data := []byte("streamed, no size sentinel, but a zip64 extra record is present regardless")
	compressed, crc := compressForTest(t, data, Deflate)
	extra := buildZip64Extra(0, 0, false, false) // placeholder record: just the tag, no size payload yet

	raw := assembleLocalEntry("xl/worksheets/sheet1.xml", compressed, crc, len(data), Deflate, true,
		0, 0, extra, true)

	w := New(bytes.NewReader(raw))

	_, entry, err := w.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	readAllAndFinish(t, entry, data)
}

// TestNeedsWideDescriptor pins the boundary of needsWideDescriptor's
// fallback rule directly, independent of any archive plumbing: a real
// 32-bit size field tops out at zip32SizeSentinel-1, so that value must
// still read as narrow while zip32SizeSentinel itself (in either the
// uncompressed or the compressed count alone) must read as wide. A real
// end-to-end fixture that actually crosses this ~4GiB boundary isn't
// practical in a test (see TestWalker_Zip64NonStreaming's comment for
// the same tradeoff elsewhere in this file), so this is the boundary's
// only direct coverage; TestEntry_TracksTrueSizesForFallback below
// covers the inputs it's fed in a real read.
func TestNeedsWideDescriptor(t *testing.T) {
	const justUnder = zip32SizeSentinel - 1

	tests := []struct {
		name                     string
		uncompressed, compressed uint64
		want                     bool
	}{
		{"both well under", 100, 100, false},
		{"uncompressed just under threshold", justUnder, 100, false},
		{"compressed just under threshold", 100, justUnder, false},
		{"uncompressed at threshold", zip32SizeSentinel, 100, true},
		{"compressed at threshold", 100, zip32SizeSentinel, true},
		{"both at threshold", zip32SizeSentinel, zip32SizeSentinel, true},
	}

	for _, tt := range tests {
		if got := needsWideDescriptor(tt.uncompressed, tt.compressed); got != tt.want {
			t.Errorf("%s: needsWideDescriptor(%d, %d) = %v, want %v",
				tt.name, tt.uncompressed, tt.compressed, got, tt.want)
		}
	}
}

// TestEntry_TracksTrueSizesForFallback pins the mechanism
// needsWideDescriptor's fallback actually depends on: that
// Entry.uncompressedN and Entry.compressedCounter.n reflect the entry's
// real sizes exactly, not an estimate, once it's been fully read.
// uncompressedN is just the running total Read hands back;
// compressedCounter counts only what the decompressor actually consumed
// from the underlying stream (per the io.ByteReader exact-consumption
// property New's doc comment relies on) -- this test checks that count
// against the real compressed length independently computed by
// compressForTest, not just against "some positive number."
func TestEntry_TracksTrueSizesForFallback(t *testing.T) {
	data := []byte(strings.Repeat("true size tracking for the Zip64 fallback ", 50))
	compressed, crc := compressForTest(t, data, Deflate)

	// No header signal at all: ordinary streaming sizes (0), no extra
	// field -- exactly the shape a genuinely unknown-size streamed entry
	// has, and the one case where the fallback is ever consulted.
	raw := assembleLocalEntry("xl/worksheets/sheet1.xml", compressed, crc, len(data), Deflate, true,
		0, 0, nil, false)

	w := New(bytes.NewReader(raw))

	_, entry, err := w.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	readAllAndFinish(t, entry, data)

	if entry.uncompressedN != int64(len(data)) {
		t.Errorf("uncompressedN = %d, want %d", entry.uncompressedN, len(data))
	}
	if entry.compressedCounter == nil {
		t.Fatal("compressedCounter = nil for a streaming deflate entry, want non-nil")
	}
	if entry.compressedCounter.n != int64(len(compressed)) {
		t.Errorf("compressedCounter.n = %d, want %d", entry.compressedCounter.n, len(compressed))
	}
}

// TestWalker_Zip64Streaming_UnsignaledFindsNextEntry covers the
// realistic version of the scenario Walker.Next's doc comment
// describes: a streamed entry whose local header gives no Zip64 signal
// at all (ordinary streaming, sizes 0, no extra field -- exactly what
// archive/zip.Writer itself produces for a normal-sized streamed entry,
// and, per that doc comment, also what it produces for one that turns
// out to need Zip64, just without ever saying so). Here the entry's true
// size is small, so needsWideDescriptor's fallback correctly infers a
// narrow descriptor and Next correctly finds the entry that follows --
// proving the fallback doesn't regress the ordinary case while closing
// the gap for the case that matters (an oversized entry, which the
// boundary math in TestNeedsWideDescriptor pins directly since a real
// ~4GiB fixture isn't practical here).
func TestWalker_Zip64Streaming_UnsignaledFindsNextEntry(t *testing.T) {
	first := []byte("streamed, no header signal, but genuinely small -- the common case")
	firstCompressed, firstCRC := compressForTest(t, first, Deflate)
	firstRaw := assembleLocalEntry("xl/worksheets/sheet1.xml", firstCompressed, firstCRC, len(first), Deflate, true,
		0, 0, nil, false)

	second := []byte("a second, perfectly ordinary entry")
	secondRaw := buildLocalEntry(t, "xl/worksheets/sheet2.xml", second, Deflate, true, true)

	w := New(bytes.NewReader(append(firstRaw, secondRaw...)))

	name, entry, err := w.Next()
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if name != "xl/worksheets/sheet1.xml" {
		t.Fatalf("name = %q, want %q", name, "xl/worksheets/sheet1.xml")
	}

	readAllAndFinish(t, entry, first)

	name2, entry2, err2 := w.Next()
	if err2 != nil {
		t.Fatalf("second Next: %v", err2)
	}
	if name2 != "xl/worksheets/sheet2.xml" {
		t.Fatalf("second entry name = %q, want %q", name2, "xl/worksheets/sheet2.xml")
	}

	readAllAndFinish(t, entry2, second)
}

// TestWalker_Zip64MalformedExtraField checks a Zip64 extra record that's
// present but too short to supply every size field the header's
// sentinels ask for.
func TestWalker_Zip64MalformedExtraField(t *testing.T) {
	data := []byte("data")
	compressed, crc := compressForTest(t, data, Deflate)
	extra := buildZip64Extra(uint64(len(data)), 0, true, false) // missing the compressed-size field the sentinel requires

	raw := assembleLocalEntry("part.xml", compressed, crc, len(data), Deflate, false,
		zip32SizeSentinel, zip32SizeSentinel, extra, false)

	w := New(bytes.NewReader(raw))

	_, _, err := w.Next()
	if err == nil {
		t.Fatal("Next() = nil error on a malformed Zip64 extra field, want an error")
	}
}
