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
// wide, independent of whether the header signaled Zip64 at all (needed
// to construct the "no signal, but the descriptor is actually wide"
// ambiguous case). compressed/crc/dataLen come from compressForTest.
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

// TestWalker_Zip64Streaming_UnsignaledAmbiguityTruncatesArchive
// documents the one genuine gap this walker has under the default
// Zip64Auto mode (see Walker.Next's doc comment): archive/zip.Writer
// itself can stream an entry that turns out to need Zip64 without ever
// signaling that in the local header (no sentinel, no extra record).
// This isn't caught by the misread entry's own CRC-32 check -- CRC is
// always the descriptor's first field, unaffected by the width of the
// size fields that follow it -- so the mistake surfaces later and
// worse: the leftover trailer bytes it leaves unconsumed swallow
// whatever comes next, so a second, perfectly valid entry after it goes
// entirely undetected (Next silently reports no more entries) rather
// than raising an error. This test pins that actual (regrettable but
// honest) default behavior rather than asserting a clean failure that
// doesn't happen. See TestWalker_Zip64Streaming_ForcedWideResolvesAmbiguity
// for how Zip64Force64 fixes this.
func TestWalker_Zip64Streaming_UnsignaledAmbiguityTruncatesArchive(t *testing.T) {
	first := []byte("streamed, actually wide descriptor, but nothing in the header says so")
	firstCompressed, firstCRC := compressForTest(t, first, Deflate)
	firstRaw := assembleLocalEntry("xl/worksheets/sheet1.xml", firstCompressed, firstCRC, len(first), Deflate, true,
		0, 0, nil, true) // no extra field at all; wideDescriptor=true regardless

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

	readAllAndFinish(t, entry, first) // the misread entry's own CRC check still happens to pass

	name2, entry2, err2 := w.Next()
	if err2 != nil || entry2 != nil {
		t.Fatalf("Next after an unsignaled Zip64 entry = (%q, %v, %v), want (\"\", nil, nil) -- "+
			"the documented limitation is that the archive silently appears to end here instead of erroring",
			name2, entry2, err2)
	}
}

// TestWalker_Zip64Streaming_ForcedWideResolvesAmbiguity uses a fixture
// like TestWalker_Zip64Streaming_UnsignaledAmbiguityTruncatesArchive's,
// but constructs the Walker with WithZip64Mode(Zip64Force64) instead of
// the default -- and confirms this actually resolves the ambiguity
// correctly: not just does the first entry still read and CRC-check
// fine, but a second entry after it, which the default mode silently
// swallows, is now found and read correctly too.
//
// Zip64Force64 is a whole-Walker setting, not a per-entry one (a
// forward-only reader has no way to ask "is this specific entry the one
// that needs it"), so both entries here are given a wide descriptor --
// representing an archive where every unsignaled streamed entry
// genuinely needs wide framing, which is the scenario this mode is for.
func TestWalker_Zip64Streaming_ForcedWideResolvesAmbiguity(t *testing.T) {
	first := []byte("streamed, actually wide descriptor, but nothing in the header says so")
	firstCompressed, firstCRC := compressForTest(t, first, Deflate)
	firstRaw := assembleLocalEntry("xl/worksheets/sheet1.xml", firstCompressed, firstCRC, len(first), Deflate, true,
		0, 0, nil, true) // no extra field at all; wideDescriptor=true regardless

	second := []byte("a second entry, also using a wide descriptor with no header signal")
	secondCompressed, secondCRC := compressForTest(t, second, Deflate)
	secondRaw := assembleLocalEntry("xl/worksheets/sheet2.xml", secondCompressed, secondCRC, len(second), Deflate, true,
		0, 0, nil, true)

	w := New(bytes.NewReader(append(firstRaw, secondRaw...)), WithZip64Mode(Zip64Force64))

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
	if entry2 == nil {
		t.Fatal("second Next: entry = nil, want the second entry to be found (Zip64Force64 should resolve the ambiguity)")
	}
	if name2 != "xl/worksheets/sheet2.xml" {
		t.Fatalf("name = %q, want %q", name2, "xl/worksheets/sheet2.xml")
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
