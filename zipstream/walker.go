// Package zipstream reads a ZIP archive's local entries strictly in the
// order they appear, without ever consulting the central directory — so
// it works from a plain io.Reader (e.g. an HTTP response body) and never
// seeks backward. Unlike a reader narrowly scoped to one writer's own
// output, it accepts both of ZIP's local-entry encodings (sizes known up
// front in the local header, or deferred to a trailing data descriptor
// for writers that can't seek back to patch the header), Zip64 sizes --
// see Walker.Next's doc comment for how a streamed entry's Zip64-ness is
// resolved even when its local header gives no signal at all -- and,
// natively, Store and Deflate compression; register a Decompressor via
// WithDecompressor for any other method, or to override either of those
// two.
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

	decompressors map[uint16]Decompressor
}

// Option configures New. See WithDecompressor.
type Option func(*options)

type options struct {
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

	return &Walker{br: bufio.NewReader(r), decompressors: o.decompressors}
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
// authoritative.
//
// Streamed entries (general-purpose bit 3, an unknown size deferred to
// the trailing data descriptor) are trickier: when archive/zip.Writer
// itself streams an entry to a plain io.Writer and only discovers after
// the fact that it needs Zip64, it does not go back and signal that in
// the local header at all ("too late anyway", per that package's own
// writeDataDescriptor comment) — the local header alone gives a
// forward-only reader no way to know the trailing descriptor will use
// 8-byte size fields instead of 4-byte ones.
//
// When the header does signal Zip64 (a sentinel size, or a Zip64 record
// present at all, even an empty placeholder one some writers use
// precisely to flag this before sizes are known) that signal is trusted
// outright, since it can come from a writer that genuinely wants a wide
// descriptor regardless of actual size (e.g. Python's zipfile with
// force_zip64=True on a small file) — actual size can't override it.
//
// When the header gives no signal at all, Next falls back to the
// entry's true sizes instead of guessing: decompression finishes before
// Finish reads the trailer, and by then both the true uncompressed size
// (Entry.uncompressedN, just the running total of bytes Read handed
// back) and the true compressed size (Entry.compressedCounter, which
// counts exactly what the decompressor consumed — no more, no less, per
// the io.ByteReader guarantee described on New) are known with
// certainty, not estimated. needsWideDescriptor then applies the same
// rule the format itself uses for the sentinel: a real 32-bit field
// tops out at zip32SizeSentinel-1, so a true size of zip32SizeSentinel
// or more could only have been encoded with 8-byte fields. This is
// exactly the scenario archive/zip.Writer's unsignaled streaming
// produces (its choice of descriptor width is itself driven by whether
// the real accumulated size overflowed 32 bits), so it resolves the
// ambiguity with certainty for every writer that behaves rationally
// about it. It would only be fooled by a writer that both omits any
// header signal and still chooses wide descriptor fields for data that
// didn't need them — a combination no known writer produces (a writer
// deliberately choosing Zip64, per the force_zip64 case above, signals
// it in the header precisely so a reader doesn't have to guess).
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
	}

	var (
		src               io.Reader
		compressedCounter *countingByteReader
	)

	// A streaming entry's compressed source is wrapped to count bytes
	// actually consumed, regardless of whether the header already
	// signals Zip64: it's needed only as Entry.Finish's fallback signal
	// when the header gave no signal at all (see Next's "Zip64"
	// section), but tracking it is cheap enough to do unconditionally
	// rather than threading that condition through every branch below.
	if streaming {
		compressedCounter = &countingByteReader{br: w.br}
	}

	if dcomp, ok := w.decompressors[method]; ok {
		var compressedSrc io.Reader = w.br

		if streaming {
			// For a streaming entry, no size is available to bound the
			// input with -- registering for Store lifts the restriction
			// below precisely because it hands the decompressor the raw
			// shared stream and trusts it to self-terminate on its own,
			// the same way flate.Reader does for Deflate.
			compressedSrc = compressedCounter
		} else {
			// The compressed size is known up front for a non-streaming
			// entry (from the header, or resolved via Zip64 above) --
			// bound the decompressor's input to exactly that, so a
			// decompressor that (unlike flate) doesn't self-terminate
			// can't accidentally consume into whatever entry follows.
			compressedSrc = io.LimitReader(w.br, int64(compSize))
		}

		src = dcomp(compressedSrc)
	} else {
		switch method {
		case Deflate:
			if streaming {
				src = flate.NewReader(compressedCounter)
			} else {
				src = flate.NewReader(w.br)
			}
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
		name:              name,
		br:                w.br,
		src:               src,
		hasher:            crc32.NewIEEE(),
		streaming:         streaming,
		zip64:             zip64,
		compressedCounter: compressedCounter,
		wantCRC:           crc32Val,
	}
	w.cur = e

	return name, e, nil
}

// countingByteReader wraps a *bufio.Reader, counting bytes actually
// consumed by its caller -- not bytes the bufio.Reader itself may have
// prefetched from its underlying source into its internal buffer.
// Implementing both io.Reader and io.ByteReader lets it stand in for br
// as a decompressor's source without losing the exact-consumption
// property New's doc comment relies on (flate.Reader, like any
// self-terminating decompressor, only reads exactly as many bytes as
// the compressed stream needs when its source is an io.ByteReader),
// while letting Walker.Next learn a streamed entry's true compressed
// size once decompression finishes.
type countingByteReader struct {
	br *bufio.Reader
	n  int64
}

func (c *countingByteReader) Read(p []byte) (int, error) {
	n, err := c.br.Read(p)
	c.n += int64(n)

	return n, err
}

func (c *countingByteReader) ReadByte() (byte, error) {
	b, err := c.br.ReadByte()
	if err == nil {
		c.n++
	}

	return b, err
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
	zip64     bool // local header explicitly signaled Zip64 for the trailing descriptor; see Walker.Next

	// compressedCounter is non-nil only for a streaming entry, and tracks
	// its true compressed size for needsWideDescriptor's fallback when
	// zip64 above is false (the header gave no signal at all).
	// uncompressedN is that same fallback's other input, tracked
	// unconditionally by Read since it costs nothing extra to maintain.
	compressedCounter *countingByteReader
	uncompressedN     int64

	wantCRC  uint32
	drained  bool
	finished bool
}

func (e *Entry) Read(p []byte) (int, error) {
	n, err := e.src.Read(p)
	if n > 0 {
		e.hasher.Write(p[:n])
		e.uncompressedN += int64(n)
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
		// The local header may have said nothing about Zip64 at all (see
		// Next's "Zip64" section) -- in that case, fall back to the
		// entry's now-known true sizes rather than assuming non-Zip64.
		zip64 := e.zip64
		if !zip64 {
			var compressedN int64
			if e.compressedCounter != nil {
				compressedN = e.compressedCounter.n
			}

			zip64 = needsWideDescriptor(uint64(e.uncompressedN), uint64(compressedN))
		}

		// Non-Zip64: crc32 + compressed size + uncompressed size, each a
		// uint32 (12 bytes). Zip64: the two sizes widen to uint64 (20
		// bytes total). Either way only the crc32 is actually used.
		restLen := 12
		if zip64 {
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
