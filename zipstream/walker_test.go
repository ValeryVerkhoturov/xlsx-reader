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

func TestWalker_NonStreaming(t *testing.T) {
	for _, method := range []uint16{Deflate, Store} {
		data := []byte(strings.Repeat("payload for the entry ", 20))
		raw := buildLocalEntry(t, "part.xml", data, method, false, false)

		w := New(bytes.NewReader(raw))

		name, entry, err := w.Next()
		if err != nil {
			t.Fatalf("method %d: Next: %v", method, err)
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

		if err := entry.Finish(); err != nil {
			t.Fatalf("method %d: Finish: %v", method, err)
		}

		name2, entry2, err := w.Next()
		if err != nil || entry2 != nil || name2 != "" {
			t.Fatalf("method %d: Next after last entry = (%q, %v, %v), want (\"\", nil, nil)", method, name2, entry2, err)
		}
	}
}

func TestWalker_StreamingWithAndWithoutDescriptorSignature(t *testing.T) {
	for _, sig := range []bool{true, false} {
		data := []byte("streamed entry data, no known size up front")
		raw := buildLocalEntry(t, "sheet1.xml", data, Deflate, true, sig)

		w := New(bytes.NewReader(raw))

		_, entry, err := w.Next()
		if err != nil {
			t.Fatalf("signature=%v: Next: %v", sig, err)
		}

		got, err := io.ReadAll(entry)
		if err != nil {
			t.Fatalf("signature=%v: reading entry: %v", sig, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("signature=%v: got %q, want %q", sig, got, data)
		}

		if err := entry.Finish(); err != nil {
			t.Fatalf("signature=%v: Finish: %v", sig, err)
		}
	}
}

func TestWalker_TwoEntriesInSequence(t *testing.T) {
	var raw bytes.Buffer
	raw.Write(buildLocalEntry(t, "first.xml", []byte("first payload"), Deflate, true, true))
	raw.Write(buildLocalEntry(t, "second.xml", []byte("second payload, a bit longer"), Store, false, false))

	w := New(bytes.NewReader(raw.Bytes()))

	name, entry, err := w.Next()
	if err != nil || name != "first.xml" {
		t.Fatalf("first entry: name=%q err=%v", name, err)
	}
	// Deliberately don't fully read entry here: Next() must finish it.
	_ = entry

	name, entry, err = w.Next()
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

func TestWalker_CRCMismatch(t *testing.T) {
	data := []byte("some data that will be corrupted")
	raw := buildLocalEntry(t, "part.xml", data, Deflate, false, false)

	// Flip a byte inside the compressed payload (after the 30-byte header
	// and 8-byte name) so decompression still succeeds but the CRC no
	// longer matches — this is exactly the corruption case Finish is
	// meant to catch.
	corruptAt := 30 + len("part.xml") + 2
	raw[corruptAt] ^= 0xFF

	w := New(bytes.NewReader(raw))

	_, entry, err := w.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	io.Copy(io.Discard, entry) //nolint:errcheck // decompression may itself error on corrupted input; Finish is what we're testing

	if err := entry.Finish(); err == nil {
		t.Fatal("Finish() = nil on corrupted data, want an error")
	}
}

// TestWalker_EncryptedEntryRejected checks that an entry with the
// encryption flag set (general-purpose bit 0) fails clearly up front
// rather than reading garbage and surfacing as a confusing CRC mismatch
// later.
func TestWalker_EncryptedEntryRejected(t *testing.T) {
	raw := buildLocalEntry(t, "part.xml", []byte("data"), Deflate, false, false)
	binary.LittleEndian.PutUint16(raw[6:8], binary.LittleEndian.Uint16(raw[6:8])|0x1) // set the encrypted flag

	w := New(bytes.NewReader(raw))

	_, _, err := w.Next()
	if err == nil {
		t.Fatal("Next() = nil error on an encrypted entry, want an error")
	}
	if !strings.Contains(err.Error(), "encrypt") {
		t.Errorf("err = %v, want it to mention encryption", err)
	}
}
