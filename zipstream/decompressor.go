package zipstream

import "io"

// Store and Deflate are the two compression methods this package
// understands natively, in ECMA-376/PKWARE APPNOTE's numbering — the
// same values (and names) as archive/zip's Store and Deflate constants.
// Pass either to WithDecompressor to override this package's built-in
// handling for that method.
const (
	Store   uint16 = 0
	Deflate uint16 = 8
)

// Decompressor returns a new reader that decompresses data read from r,
// which is method's compressed representation of one archive entry. Its
// Close method, if any, is called once the entry is done being read (see
// Entry.Finish); the returned reader need not verify anything itself --
// Walker already checks the decompressed data's CRC-32 as it's read.
//
// For a non-streaming entry (compressed size known up front), r is
// already bounded to exactly that many bytes, so reading it to EOF is
// always safe. For a streamed entry (general-purpose bit 3, unknown
// size), no such bound is possible: r is the raw shared stream, and the
// Decompressor -- like flate.Reader -- must know on its own where its
// data ends and stop there, reading not one byte more. Overreading in
// that case corrupts whatever entry follows, the same hazard New's doc
// comment describes for r as a whole.
type Decompressor func(r io.Reader) io.ReadCloser

// WithDecompressor registers dcomp as the decompressor for method on
// the Walker being constructed. This overrides the built-in handling
// for Store or Deflate if method is one of those, or adds support for a
// method this package doesn't know natively (e.g. bzip2, LZMA, zstd)
// otherwise. It also lifts the restriction on combining Store with a
// streamed (unknown-size) entry, since that restriction exists only
// because the built-in Store handling needs a known size — a custom
// Decompressor for method Store is free to use a self-terminating
// framing of its own instead.
//
// This mirrors the shape of (*archive/zip.Reader).RegisterDecompressor,
// scoped to one Walker instead of process-wide.
func WithDecompressor(method uint16, dcomp Decompressor) Option {
	return func(o *options) {
		if o.decompressors == nil {
			o.decompressors = make(map[uint16]Decompressor)
		}

		o.decompressors[method] = dcomp
	}
}
