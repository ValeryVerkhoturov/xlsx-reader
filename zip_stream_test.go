package xlsx

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"hash/crc32"
	"io"
	"strings"
	"testing"
)

// buildLocalEntry hand-builds the bytes for one ZIP local entry, in
// either of the two encodings real writers use: sizes known up front in
// the local header (streaming=false), or deferred to a trailing data
// descriptor (streaming=true, optionally without the descriptor's
// optional 4-byte signature — real writers disagree on including it).
// archive/zip.Writer can only ever produce the streaming encoding, so
// this is the only way to exercise the other one in tests.
func buildLocalEntry(t *testing.T, name string, data []byte, method uint16, streaming, descriptorSignature bool) []byte {
	t.Helper()

	var comp bytes.Buffer

	switch method {
	case zipMethodDeflate:
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
	case zipMethodStored:
		comp.Write(data)
	default:
		t.Fatalf("buildLocalEntry: unsupported method %d", method)
	}

	crc := crc32.ChecksumIEEE(data)

	var header [30]byte
	binary.LittleEndian.PutUint32(header[0:4], zipLocalFileHeaderSignature)
	binary.LittleEndian.PutUint16(header[4:6], 20) // version needed to extract

	var gpFlag uint16
	if streaming {
		gpFlag |= 0x8
	}
	binary.LittleEndian.PutUint16(header[6:8], gpFlag)
	binary.LittleEndian.PutUint16(header[8:10], method)

	if !streaming {
		binary.LittleEndian.PutUint32(header[14:18], crc)
		binary.LittleEndian.PutUint32(header[18:22], uint32(comp.Len()))
		binary.LittleEndian.PutUint32(header[22:26], uint32(len(data)))
	}

	binary.LittleEndian.PutUint16(header[26:28], uint16(len(name)))

	var buf bytes.Buffer
	buf.Write(header[:])
	buf.WriteString(name)
	buf.Write(comp.Bytes())

	if streaming {
		var descriptor [16]byte
		off := 0
		if descriptorSignature {
			binary.LittleEndian.PutUint32(descriptor[0:4], zipDataDescriptorSignature)
			off = 4
		}
		binary.LittleEndian.PutUint32(descriptor[off:off+4], crc)
		binary.LittleEndian.PutUint32(descriptor[off+4:off+8], uint32(comp.Len()))
		binary.LittleEndian.PutUint32(descriptor[off+8:off+12], uint32(len(data)))
		buf.Write(descriptor[:off+12])
	}

	return buf.Bytes()
}

func TestZipWalker_NonStreaming(t *testing.T) {
	for _, method := range []uint16{zipMethodDeflate, zipMethodStored} {
		data := []byte(strings.Repeat("payload for the entry ", 20))
		raw := buildLocalEntry(t, "part.xml", data, method, false, false)

		zw := newZipWalker(bytes.NewReader(raw))

		name, entry, err := zw.next()
		if err != nil {
			t.Fatalf("method %d: next: %v", method, err)
		}
		if name != "part.xml" {
			t.Fatalf("method %d: name = %q, want %q", method, name, "part.xml")
		}

		got, err := io.ReadAll(entry)
		if err != nil {
			t.Fatalf("method %d: reading entry: %v", method, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("method %d: got %q, want %q", method, got, data)
		}

		if err := entry.finish(); err != nil {
			t.Fatalf("method %d: finish: %v", method, err)
		}

		name2, entry2, err := zw.next()
		if err != nil || entry2 != nil || name2 != "" {
			t.Fatalf("method %d: next after last entry = (%q, %v, %v), want (\"\", nil, nil)", method, name2, entry2, err)
		}
	}
}

func TestZipWalker_StreamingWithAndWithoutDescriptorSignature(t *testing.T) {
	for _, sig := range []bool{true, false} {
		data := []byte("streamed entry data, no known size up front")
		raw := buildLocalEntry(t, "sheet1.xml", data, zipMethodDeflate, true, sig)

		zw := newZipWalker(bytes.NewReader(raw))

		_, entry, err := zw.next()
		if err != nil {
			t.Fatalf("signature=%v: next: %v", sig, err)
		}

		got, err := io.ReadAll(entry)
		if err != nil {
			t.Fatalf("signature=%v: reading entry: %v", sig, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("signature=%v: got %q, want %q", sig, got, data)
		}

		if err := entry.finish(); err != nil {
			t.Fatalf("signature=%v: finish: %v", sig, err)
		}
	}
}

func TestZipWalker_TwoEntriesInSequence(t *testing.T) {
	var raw bytes.Buffer
	raw.Write(buildLocalEntry(t, "first.xml", []byte("first payload"), zipMethodDeflate, true, true))
	raw.Write(buildLocalEntry(t, "second.xml", []byte("second payload, a bit longer"), zipMethodStored, false, false))

	zw := newZipWalker(bytes.NewReader(raw.Bytes()))

	name, entry, err := zw.next()
	if err != nil || name != "first.xml" {
		t.Fatalf("first entry: name=%q err=%v", name, err)
	}
	// Deliberately don't fully read entry here: next() must finish it.
	_ = entry

	name, entry, err = zw.next()
	if err != nil {
		t.Fatalf("advancing to second entry: %v", err)
	}
	if name != "second.xml" {
		t.Fatalf("second entry name = %q, want %q", name, "second.xml")
	}

	got, err := io.ReadAll(entry)
	if err != nil {
		t.Fatalf("reading second entry: %v", err)
	}
	if string(got) != "second payload, a bit longer" {
		t.Fatalf("second entry data = %q", got)
	}
}

func TestZipWalker_CRCMismatch(t *testing.T) {
	data := []byte("some data that will be corrupted")
	raw := buildLocalEntry(t, "part.xml", data, zipMethodDeflate, false, false)

	// Flip a byte inside the compressed payload (after the 30-byte header
	// and 8-byte name) so decompression still succeeds but the CRC no
	// longer matches — this is exactly the corruption case finish is
	// meant to catch.
	corruptAt := 30 + len("part.xml") + 2
	raw[corruptAt] ^= 0xFF

	zw := newZipWalker(bytes.NewReader(raw))

	_, entry, err := zw.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}

	io.Copy(io.Discard, entry) //nolint:errcheck // decompression may itself error on corrupted input; finish is what we're testing

	if err := entry.finish(); err == nil {
		t.Fatal("finish() = nil on corrupted data, want an error")
	}
}

// TestZipWalker_EncryptedEntryRejected checks that an entry with the
// encryption flag set (general-purpose bit 0) fails clearly up front
// rather than reading garbage and surfacing as a confusing CRC
// mismatch later.
func TestZipWalker_EncryptedEntryRejected(t *testing.T) {
	raw := buildLocalEntry(t, "part.xml", []byte("data"), zipMethodDeflate, false, false)
	binary.LittleEndian.PutUint16(raw[6:8], binary.LittleEndian.Uint16(raw[6:8])|0x1) // set the encrypted flag

	zw := newZipWalker(bytes.NewReader(raw))

	_, _, err := zw.next()
	if err == nil {
		t.Fatal("next() = nil error on an encrypted entry, want an error")
	}
	if !strings.Contains(err.Error(), "encrypt") {
		t.Errorf("err = %v, want it to mention encryption", err)
	}
}

// TestZipWalker_Zip64SentinelWithoutExtraField checks a header that
// flags Zip64 (a sentinel size) but supplies no Zip64 extra field to
// resolve it against -- malformed, so this must fail clearly rather
// than silently treating the sentinel as a literal 4-gigabyte size.
func TestZipWalker_Zip64SentinelWithoutExtraField(t *testing.T) {
	raw := buildLocalEntry(t, "part.xml", []byte("data"), zipMethodDeflate, false, false)
	binary.LittleEndian.PutUint32(raw[18:22], zip32SizeSentinel) // compressed-size Zip64 marker

	zw := newZipWalker(bytes.NewReader(raw))

	_, _, err := zw.next()
	if err == nil {
		t.Fatal("next() = nil error on an unresolved Zip64 marker, want an error")
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
	case zipMethodDeflate:
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
	case zipMethodStored:
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

func readAllAndFinish(t *testing.T, entry *zipEntry, want []byte) {
	t.Helper()

	got, err := io.ReadAll(entry)
	if err != nil {
		t.Fatalf("reading entry: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	if err := entry.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

// TestZipWalker_Zip64NonStreaming covers the common, fully unambiguous
// case: a real writer (Excel, LibreOffice, anything targeting a
// seekable destination) knows its sizes up front and signals Zip64 with
// sentinel header fields plus a matching extra field record. The
// payload here is deliberately tiny -- a real >4GB fixture isn't
// practical in a test -- which is itself realistic: some writers (e.g.
// Python's zipfile with force_zip64=True) always use Zip64 regardless
// of actual size.
func TestZipWalker_Zip64NonStreaming(t *testing.T) {
	data := []byte("zip64-flagged but tiny payload; sizes come from the extra field, not the header")
	compressed, crc := compressForTest(t, data, zipMethodDeflate)
	extra := buildZip64Extra(uint64(len(data)), uint64(len(compressed)), true, true)

	raw := assembleLocalEntry("part.xml", compressed, crc, len(data), zipMethodDeflate, false,
		zip32SizeSentinel, zip32SizeSentinel, extra, false)

	zw := newZipWalker(bytes.NewReader(raw))

	name, entry, err := zw.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if name != "part.xml" {
		t.Fatalf("name = %q, want %q", name, "part.xml")
	}

	readAllAndFinish(t, entry, data)
}

// TestZipWalker_Zip64NonStreaming_OnlyOneSizeFlagged checks the
// "only if flagged" consumption rule: when just the compressed size is
// a sentinel, the extra field supplies only that field, and the
// (non-sentinel) uncompressed size in the header itself is left alone.
func TestZipWalker_Zip64NonStreaming_OnlyOneSizeFlagged(t *testing.T) {
	data := []byte("only the compressed size needs Zip64 resolution here")
	compressed, crc := compressForTest(t, data, zipMethodDeflate)
	extra := buildZip64Extra(0, uint64(len(compressed)), false, true)

	raw := assembleLocalEntry("part.xml", compressed, crc, len(data), zipMethodDeflate, false,
		zip32SizeSentinel, uint32(len(data)), extra, false)

	zw := newZipWalker(bytes.NewReader(raw))

	_, entry, err := zw.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}

	readAllAndFinish(t, entry, data)
}

// TestZipWalker_Zip64Streaming_SignaledBySentinel covers a streamed
// entry that explicitly signals Zip64 (sentinel sizes plus an extra
// field record) in its local header -- unambiguous, so the trailing
// data descriptor is correctly read as 8-byte-wide.
func TestZipWalker_Zip64Streaming_SignaledBySentinel(t *testing.T) {
	data := []byte("streamed, zip64-sentinel'd, wide trailing data descriptor")
	compressed, crc := compressForTest(t, data, zipMethodDeflate)
	extra := buildZip64Extra(uint64(len(data)), uint64(len(compressed)), true, true)

	raw := assembleLocalEntry("xl/worksheets/sheet1.xml", compressed, crc, len(data), zipMethodDeflate, true,
		zip32SizeSentinel, zip32SizeSentinel, extra, true)

	zw := newZipWalker(bytes.NewReader(raw))

	_, entry, err := zw.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}

	readAllAndFinish(t, entry, data)
}

// TestZipWalker_Zip64Streaming_SignaledByExtraFieldOnly covers a
// streamed entry whose header sizes are the ordinary 0 (not the
// sentinel) but which still carries a Zip64 extra field record -- how a
// well-behaved streaming writer flags Zip64 before it knows the real
// sizes. Presence of the record alone is treated as a reliable signal
// that the trailing descriptor is wide.
func TestZipWalker_Zip64Streaming_SignaledByExtraFieldOnly(t *testing.T) {
	data := []byte("streamed, no size sentinel, but a zip64 extra record is present regardless")
	compressed, crc := compressForTest(t, data, zipMethodDeflate)
	extra := buildZip64Extra(0, 0, false, false) // placeholder record: just the tag, no size payload yet

	raw := assembleLocalEntry("xl/worksheets/sheet1.xml", compressed, crc, len(data), zipMethodDeflate, true,
		0, 0, extra, true)

	zw := newZipWalker(bytes.NewReader(raw))

	_, entry, err := zw.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}

	readAllAndFinish(t, entry, data)
}

// TestZipWalker_Zip64Streaming_UnsignaledAmbiguityTruncatesArchive
// documents the one genuine gap this walker has (see zipWalker.next's
// doc comment): archive/zip.Writer itself can stream an entry that
// turns out to need Zip64 without ever signaling that in the local
// header (no sentinel, no extra record). This isn't caught by the
// misread entry's own CRC-32 check -- CRC is always the descriptor's
// first field, unaffected by the width of the size fields that follow
// it -- so the mistake surfaces later and worse: the leftover trailer
// bytes it leaves unconsumed swallow whatever comes next, so a second,
// perfectly valid entry after it goes entirely undetected (next
// silently reports no more entries) rather than raising an error. This
// test pins that actual (regrettable but honest) behavior rather than
// asserting a clean failure that doesn't happen.
func TestZipWalker_Zip64Streaming_UnsignaledAmbiguityTruncatesArchive(t *testing.T) {
	first := []byte("streamed, actually wide descriptor, but nothing in the header says so")
	firstCompressed, firstCRC := compressForTest(t, first, zipMethodDeflate)
	firstRaw := assembleLocalEntry("xl/worksheets/sheet1.xml", firstCompressed, firstCRC, len(first), zipMethodDeflate, true,
		0, 0, nil, true) // no extra field at all; wideDescriptor=true regardless

	second := []byte("a second, perfectly ordinary entry")
	secondRaw := buildLocalEntry(t, "xl/worksheets/sheet2.xml", second, zipMethodDeflate, true, true)

	zw := newZipWalker(bytes.NewReader(append(firstRaw, secondRaw...)))

	name, entry, err := zw.next()
	if err != nil {
		t.Fatalf("first next: %v", err)
	}
	if name != "xl/worksheets/sheet1.xml" {
		t.Fatalf("name = %q, want %q", name, "xl/worksheets/sheet1.xml")
	}

	readAllAndFinish(t, entry, first) // the misread entry's own CRC check still happens to pass

	name2, entry2, err2 := zw.next()
	if err2 != nil || entry2 != nil {
		t.Fatalf("next after an unsignaled Zip64 entry = (%q, %v, %v), want (\"\", nil, nil) -- "+
			"the documented limitation is that the archive silently appears to end here instead of erroring",
			name2, entry2, err2)
	}
}

// TestZipWalker_Zip64MalformedExtraField checks a Zip64 extra record
// that's present but too short to supply every size field the header's
// sentinels ask for.
func TestZipWalker_Zip64MalformedExtraField(t *testing.T) {
	data := []byte("data")
	compressed, crc := compressForTest(t, data, zipMethodDeflate)
	extra := buildZip64Extra(uint64(len(data)), 0, true, false) // missing the compressed-size field the sentinel requires

	raw := assembleLocalEntry("part.xml", compressed, crc, len(data), zipMethodDeflate, false,
		zip32SizeSentinel, zip32SizeSentinel, extra, false)

	zw := newZipWalker(bytes.NewReader(raw))

	_, _, err := zw.next()
	if err == nil {
		t.Fatal("next() = nil error on a malformed Zip64 extra field, want an error")
	}
}
