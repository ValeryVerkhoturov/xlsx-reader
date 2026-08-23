package zipstream

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"strings"
	"testing"
)

// xorDecompressor is a trivial reversible "compression" scheme for
// tests: it XORs every byte with key. Applying it twice recovers the
// original data, which is all a test needs to prove a Decompressor was
// actually invoked (as opposed to the bytes simply passing through
// unmodified, which the built-in Store handling would also produce).
func xorDecompressor(key byte) Decompressor {
	return func(r io.Reader) io.ReadCloser {
		return io.NopCloser(&xorReader{r: r, key: key})
	}
}

type xorReader struct {
	r   io.Reader
	key byte
}

func (x *xorReader) Read(p []byte) (int, error) {
	n, err := x.r.Read(p)
	for i := 0; i < n; i++ {
		p[i] ^= x.key
	}
	return n, err
}

// TestWalker_CustomDecompressorForUnknownMethod registers a Decompressor
// for a method (99) this package has no built-in support for, on a
// non-streaming entry, and checks it's used to recover the original
// data and that the CRC-32 check (computed over the decompressed
// output) passes.
func TestWalker_CustomDecompressorForUnknownMethod(t *testing.T) {
	const method = 99

	data := []byte("payload compressed with a method this package doesn't know natively")
	compressed := xorBytes(data, 0xAA)
	crc := crc32.ChecksumIEEE(data)

	raw := assembleLocalEntry("part.xml", compressed, crc, len(data), method, false,
		uint32(len(compressed)), uint32(len(data)), nil, false)

	w := New(bytes.NewReader(raw), WithDecompressor(method, xorDecompressor(0xAA)))

	name, entry, err := w.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if name != "part.xml" {
		t.Fatalf("name = %q, want %q", name, "part.xml")
	}

	readAllAndFinish(t, entry, data)
}

// TestWalker_CustomDecompressorOverridesBuiltinStore checks that
// registering a Decompressor for method Store takes priority over the
// built-in Store handling. The fixture's "compressed" bytes are XOR'd,
// so the built-in (identity) Store handling would return them
// unmodified -- which would fail the CRC-32 check, since that's
// computed against the original, un-XOR'd data. Only if the registered
// Decompressor is actually consulted does the data (and its CRC) come
// out right.
func TestWalker_CustomDecompressorOverridesBuiltinStore(t *testing.T) {
	data := []byte("payload that only checks out if the override actually took priority")
	compressed := xorBytes(data, 0x5A)
	crc := crc32.ChecksumIEEE(data)

	raw := assembleLocalEntry("part.xml", compressed, crc, len(data), Store, false,
		uint32(len(compressed)), uint32(len(data)), nil, false)

	t.Run("without override, CRC check fails", func(t *testing.T) {
		w := New(bytes.NewReader(raw))

		_, entry, err := w.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}

		io.Copy(io.Discard, entry) //nolint:errcheck // reading raw XOR'd bytes never itself errors; Finish is what's being tested

		if err := entry.Finish(); err == nil {
			t.Fatal("Finish() = nil without the override, want a CRC mismatch error")
		}
	})

	t.Run("with override, data and CRC check out", func(t *testing.T) {
		w := New(bytes.NewReader(raw), WithDecompressor(Store, xorDecompressor(0x5A)))

		_, entry, err := w.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}

		readAllAndFinish(t, entry, data)
	})
}

// lengthPrefixedDecompressor decodes a trivial self-terminating framing
// -- a 4-byte little-endian length prefix followed by that many raw
// bytes -- entirely on its own, without being told the size externally.
// It exists to prove a registered Decompressor can correctly handle a
// *streamed* entry (general-purpose bit 3, no size known up front from
// the archive itself), which the built-in Store handling categorically
// refuses.
func lengthPrefixedDecompressor() Decompressor {
	return func(r io.Reader) io.ReadCloser {
		var lenBuf [4]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return io.NopCloser(readErrorer{err})
		}

		n := binary.LittleEndian.Uint32(lenBuf[:])

		return io.NopCloser(io.LimitReader(r, int64(n)))
	}
}

type readErrorer struct{ err error }

func (e readErrorer) Read([]byte) (int, error) { return 0, e.err }

// TestWalker_CustomDecompressorEnablesStreamingForStore registers a
// self-terminating Decompressor for method Store on a *streamed* entry
// -- something the built-in Store handling rejects outright, since it
// has no size to bound an io.LimitReader with. Registering an override
// lifts that restriction, since it's then the Decompressor's own job
// (like flate.Reader's) to know where its data ends.
func TestWalker_CustomDecompressorEnablesStreamingForStore(t *testing.T) {
	data := []byte("streamed entry, self-terminating length-prefixed custom framing")

	var lenPrefix [4]byte
	binary.LittleEndian.PutUint32(lenPrefix[:], uint32(len(data)))
	compressed := append(lenPrefix[:], data...)

	crc := crc32.ChecksumIEEE(data)

	raw := assembleLocalEntry("xl/worksheets/sheet1.xml", compressed, crc, len(data), Store, true,
		0, 0, nil, false)

	w := New(bytes.NewReader(raw), WithDecompressor(Store, lengthPrefixedDecompressor()))

	name, entry, err := w.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if name != "xl/worksheets/sheet1.xml" {
		t.Fatalf("name = %q, want %q", name, "xl/worksheets/sheet1.xml")
	}

	readAllAndFinish(t, entry, data)
}

// TestWalker_UnregisteredUnknownMethodErrors checks that an entry using
// a method this package doesn't know, with no Decompressor registered
// for it, fails clearly and points at WithDecompressor rather than
// silently misreading the entry.
func TestWalker_UnregisteredUnknownMethodErrors(t *testing.T) {
	const method = 99

	data := []byte("data")
	raw := assembleLocalEntry("part.xml", data, crc32.ChecksumIEEE(data), len(data), method, false,
		uint32(len(data)), uint32(len(data)), nil, false)

	w := New(bytes.NewReader(raw))

	_, _, err := w.Next()
	if err == nil {
		t.Fatal("Next() = nil error on an unregistered, unrecognized compression method, want an error")
	}
	if !strings.Contains(err.Error(), "WithDecompressor") {
		t.Errorf("err = %v, want it to mention WithDecompressor", err)
	}
}

func xorBytes(data []byte, key byte) []byte {
	out := make([]byte, len(data))
	for i, b := range data {
		out[i] = b ^ key
	}
	return out
}
