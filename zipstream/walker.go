// Package zipstream reads a ZIP archive's local entries strictly in the
// order they appear, without ever consulting the central directory — so
// it works from a plain io.Reader (e.g. an HTTP response body) and never
// seeks backward. Unlike a reader narrowly scoped to one writer's own
// output, it accepts both of ZIP's local-entry encodings (sizes known up
// front in the local header, or deferred to a trailing data descriptor
// for writers that can't seek back to patch the header), Zip64 sizes --
// see Walker.Next's doc comment for the one genuine ambiguity that
// remains for a forward-only reader, and Zip64Mode for the escape hatch
// it offers -- and, natively, Store and Deflate compression; register a
// Decompressor via WithDecompressor for any other method, or to override
// either of those two.
package zipstream

import (
	"bufio"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
)

// Local-file-header constants for the general forward-only walker below.
const (
	zipLocalFileHeaderSignature = 0x04034b50
	zipDataDescriptorSignature  = 0x08074b50
)

// Walker reads a ZIP archive's local entries strictly in the order they
// appear, without ever consulting the central directory — so it works
// from a plain io.Reader (e.g. an HTTP response body) and never seeks
// backward.
type Walker struct {
	br  *bufio.Reader
	cur *Entry // the still-open entry from the last Next call, if any

	zip64Mode     Zip64Mode
	decompressors map[uint16]Decompressor
}

// Option configures New. See WithZip64Mode and WithDecompressor.
type Option func(*options)

type options struct {
	zip64Mode     Zip64Mode
	decompressors map[uint16]Decompressor
}

// New wraps r for entry-at-a-time reading. r is read through a single
// bufio.Reader for the Walker's whole lifetime and must not be read from
// elsewhere: a decompressor (built-in or, via WithDecompressor, custom)
// only consumes exactly the bytes a compressed stream needs when its
// source implements io.ByteReader, otherwise it can read ahead into
// whatever follows with no way to recover the bytes it took, so every
// read (headers, entry data, trailers) has to share one buffered reader.
func New(r io.Reader, opts ...Option) *Walker {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	return &Walker{br: bufio.NewReader(r), zip64Mode: o.zip64Mode, decompressors: o.decompressors}
}

// Next finishes the previous entry (discarding any data the caller
// didn't read and validating its CRC-32), then reads the next local
// entry's header and returns a reader for its decompressed data. A nil
// entry with a nil error means there are no more local entries — the
// central directory or end of input follows.
//
// # Zip64
//
// A local header whose 32-bit compressed and/or uncompressed size field
// is the sentinel zip32SizeSentinel is resolved against a Zip64
// extended-information record (zip64ExtraID) in the entry's own extra
// field, in the same uncompressed-then-compressed order and
// only-if-flagged rule Go's own archive/zip reader uses — this is
// unambiguous and handles how Excel, LibreOffice, and virtually every
// writer targeting a seekable destination signals Zip64. It is always
// authoritative, regardless of Zip64Mode.
//
// Streamed entries (general-purpose bit 3, an unknown size deferred to
// the trailing data descriptor) are a genuine exception: when
// archive/zip.Writer itself streams an entry to a plain io.Writer and
// only discovers after the fact that it needs Zip64, it does not go
// back and signal that in the local header at all ("too late anyway",
// per that package's own writeDataDescriptor comment) — the local
// header gives a forward-only reader no way to know the trailing
// descriptor will use 8-byte size fields instead of 4-byte ones. This
// walker widens the descriptor when the local header signals Zip64 (a
// sentinel size, or a Zip64 record present at all, even an empty
// placeholder one some writers use precisely to flag this before sizes
// are known) — and otherwise, when neither signal is present, falls
// back to Zip64Mode: the default, Zip64Auto (and Zip64Force32,
// stated explicitly), assumes narrow; Zip64Force64 assumes wide.
//
// Getting that fallback wrong is worse than an error: the misread
// entry's own CRC-32 check still passes (the CRC is always the
// descriptor's first field, unaffected by the width of the size fields
// that follow it), so the mistake goes uncaught right there, leaving 8
// or 16 leftover trailer bytes unconsumed immediately after. If the
// archive happens to end there, nothing is even detectably wrong. If
// another entry follows, those leftover bytes are almost certainly
// mistaken for garbage rather than a local-file-header signature, which
// makes Next silently report "no more entries" — silently truncating
// the rest of the archive rather than raising an error. A caller who
// knows or suspects the source archive comes from a Go-authored,
// non-seekable-destination, streamed Zip64 write can use
// WithZip64Mode(Zip64Force64) to resolve this correctly instead.
//
// # Compression methods
//
// Store and Deflate are handled internally. An entry using any other
// method fails unless a Decompressor has been registered for it via
// WithDecompressor; registering one for Store or Deflate overrides the
// built-in handling for that method too. See WithDecompressor.
func (w *Walker) Next() (name string, entry *Entry, err error) {
	if w.cur != nil {
		if err := w.cur.Finish(); err != nil {
			return "", nil, err
		}

		w.cur = nil
	}

	var header [30]byte

	if _, err := io.ReadFull(w.br, header[:]); err != nil {
		if err == io.EOF {
			return "", nil, nil
		}

		return "", nil, fmt.Errorf("zipstream: reading archive: %w", err)
	}

	if binary.LittleEndian.Uint32(header[0:4]) != zipLocalFileHeaderSignature {
		// Not a local file header: either the central directory or true
		// EOF follows. Either way, there are no more entries to read.
		return "", nil, nil
	}

	var (
		gpFlag       = binary.LittleEndian.Uint16(header[6:8])
		method       = binary.LittleEndian.Uint16(header[8:10])
		crc32Val     = binary.LittleEndian.Uint32(header[14:18])
		compSize32   = binary.LittleEndian.Uint32(header[18:22])
		uncompSize32 = binary.LittleEndian.Uint32(header[22:26])
		nameLen      = binary.LittleEndian.Uint16(header[26:28])
		extraLen     = binary.LittleEndian.Uint16(header[28:30])
	)

	streaming := gpFlag&0x8 != 0

	nameBytes := make([]byte, nameLen)
	if _, err := io.ReadFull(w.br, nameBytes); err != nil {
		return "", nil, fmt.Errorf("zipstream: reading archive entry name: %w", err)
	}

	name = string(nameBytes)

	if gpFlag&0x1 != 0 {
		return "", nil, fmt.Errorf("zipstream: archive entry %q is encrypted, which is not supported", name)
	}

	var extra []byte
	if extraLen > 0 {
		extra = make([]byte, extraLen)
		if _, err := io.ReadFull(w.br, extra); err != nil {
			return "", nil, fmt.Errorf("zipstream: reading archive entry %q extra field: %w", name, err)
		}
	}

	needUncompSize := uncompSize32 == zip32SizeSentinel
	needCompSize := compSize32 == zip32SizeSentinel

	compSize := uint64(compSize32)
	zip64 := needUncompSize || needCompSize

	if zip64 {
		_, c, sawRecord, perr := parseZip64Extra(extra, needUncompSize, needCompSize)
		if perr != nil {
			return "", nil, fmt.Errorf("zipstream: archive entry %q: %w", name, perr)
		}

		if !sawRecord {
			return "", nil, fmt.Errorf("zipstream: archive entry %q declares a Zip64 size but has no Zip64 extra field", name)
		}

		// The uncompressed size (returned but unused here) plays no
		// further role: flate.Reader is self-terminating and the
		// stored-method reader below only needs the compressed size.
		if needCompSize {
			compSize = c
		}
	} else if _, _, sawRecord, _ := parseZip64Extra(extra, false, false); sawRecord {
		// No sentinel size, but the writer included a Zip64 record
		// anyway -- some streaming writers do this specifically to flag
		// Zip64 before the real sizes are known. Treat it as a reliable
		// signal that the trailing data descriptor (if any) is widened.
		zip64 = true
	} else if w.zip64Mode == Zip64Force64 {
		// No signal at all in the local header -- the one genuine
		// ambiguity a forward-only reader can't resolve from the
		// archive's own bytes. Zip64Force64 opts into assuming a wide
		// trailing descriptor here instead of the Zip64Auto/
		// Zip64Force32 default.
		zip64 = true
	}

	var src io.Reader

	if dcomp, ok := w.decompressors[method]; ok {
		var compressedSrc io.Reader = w.br

		if !streaming {
			// The compressed size is known up front for a non-streaming
			// entry (from the header, or resolved via Zip64 above) --
			// bound the decompressor's input to exactly that, so a
			// decompressor that (unlike flate) doesn't self-terminate
			// can't accidentally consume into whatever entry follows.
			compressedSrc = io.LimitReader(w.br, int64(compSize))
		}

		// For a streaming entry, no size is available to bound the
		// input with -- registering for Store lifts the restriction
		// below precisely because it hands the decompressor the raw
		// shared stream and trusts it to self-terminate on its own, the
		// same way flate.Reader does for Deflate.
		src = dcomp(compressedSrc)
	} else {
		switch method {
		case Deflate:
			src = flate.NewReader(w.br)
		case Store:
			if streaming {
				return "", nil, fmt.Errorf("zipstream: archive entry %q combines stored compression with a streamed (unknown-size) encoding, which is not supported; register a Decompressor for method %d via WithDecompressor if your writer's stored streaming framing is self-terminating", name, Store)
			}

			src = io.LimitReader(w.br, int64(compSize))
		default:
			return "", nil, fmt.Errorf("zipstream: archive entry %q uses unsupported compression method %d; register a Decompressor for it via WithDecompressor", name, method)
		}
	}

	e := &Entry{
		name:      name,
		br:        w.br,
		src:       src,
		hasher:    crc32.NewIEEE(),
		streaming: streaming,
		zip64:     zip64,
		wantCRC:   crc32Val,
	}
	w.cur = e

	return name, e, nil
}

// Entry is one archive entry's decompressed data, readable exactly once
// via Read. Call Finish (or simply call Walker.Next again, which does so
// on the caller's behalf) once done reading — whether the entry was read
// to completion or abandoned partway through — to validate its CRC-32
// and leave the underlying reader positioned at the next entry.
type Entry struct {
	name      string
	br        *bufio.Reader
	src       io.Reader
	hasher    hash.Hash32
	streaming bool
	zip64     bool // widens the trailing data descriptor's size fields to 8 bytes each; see Walker.Next
	wantCRC   uint32
	drained   bool
	finished  bool
}

func (e *Entry) Read(p []byte) (int, error) {
	n, err := e.src.Read(p)
	if n > 0 {
		e.hasher.Write(p[:n])
	}

	if err == io.EOF {
		e.drained = true
	}

	return n, err
}

// Finish drains any data the caller left unread, then checks the
// entry's CRC-32: read directly from the local header for a known-size
// entry, or from the trailing data descriptor for a streamed one. The
// descriptor's leading 4-byte signature is detected rather than
// required — real-world writers disagree on including it — by checking
// whether the first 4 trailer bytes match the signature; if they don't,
// those same 4 bytes are the CRC-32 field itself.
func (e *Entry) Finish() error {
	if e.finished {
		return nil
	}

	e.finished = true

	if !e.drained {
		if _, err := io.Copy(io.Discard, e); err != nil {
			return fmt.Errorf("zipstream: reading archive entry %q: %w", e.name, err)
		}
	}

	if c, ok := e.src.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return fmt.Errorf("zipstream: reading archive entry %q: %w", e.name, err)
		}
	}

	wantCRC := e.wantCRC

	if e.streaming {
		// Non-Zip64: crc32 + compressed size + uncompressed size, each a
		// uint32 (12 bytes). Zip64: the two sizes widen to uint64 (20
		// bytes total). Either way only the crc32 is actually used.
		restLen := 12
		if e.zip64 {
			restLen = 20
		}

		var first4 [4]byte

		if _, err := io.ReadFull(e.br, first4[:]); err != nil {
			return fmt.Errorf("zipstream: reading archive entry %q trailer: %w", e.name, err)
		}

		if binary.LittleEndian.Uint32(first4[:]) == zipDataDescriptorSignature {
			rest := make([]byte, restLen)

			if _, err := io.ReadFull(e.br, rest); err != nil {
				return fmt.Errorf("zipstream: reading archive entry %q trailer: %w", e.name, err)
			}

			wantCRC = binary.LittleEndian.Uint32(rest[0:4])
		} else {
			rest := make([]byte, restLen-4) // first4 already holds the crc32 itself

			if _, err := io.ReadFull(e.br, rest); err != nil {
				return fmt.Errorf("zipstream: reading archive entry %q trailer: %w", e.name, err)
			}

			wantCRC = binary.LittleEndian.Uint32(first4[:])
		}
	}

	if got := e.hasher.Sum32(); got != wantCRC {
		return fmt.Errorf("zipstream: archive entry %q failed CRC check (got %08x, want %08x); data may be corrupt", e.name, got, wantCRC)
	}

	return nil
}
