package zipstream

import (
	"encoding/binary"
	"fmt"
)

const (
	// zip64ExtraID is the extra-field record tag (ECMA-376/PKWARE APPNOTE
	// 4.5.3) carrying the real 64-bit sizes when a local header's own
	// 32-bit size field is the sentinel value zip32SizeSentinel.
	zip64ExtraID      = 0x0001
	zip32SizeSentinel = 0xFFFFFFFF
)

// Zip64Mode controls how Walker resolves the one genuinely ambiguous
// Zip64 case: a streamed entry (general-purpose bit 3) whose local
// header signals Zip64 neither via a sentinel size nor via a Zip64
// extra-field record. It has NO effect on any other case — sentinel-
// signaled and extra-field-signaled Zip64 are always honored exactly as
// the archive declares, regardless of mode; see Walker.Next's doc
// comment for why that detection is unambiguous and never overridden.
//
// It is a whole-Walker setting, set once via WithZip64Mode when the
// Walker is constructed, applying identically to every ambiguous entry
// the Walker encounters — a forward-only reader has no way to ask "is
// this specific entry the one that actually needs wide framing" on a
// per-entry basis. Zip64Force64 is therefore only appropriate when every
// unsignaled streamed entry in the archive genuinely needs wide framing;
// forcing it on an archive that mixes ordinary small streamed entries
// with one that needs Zip64 will misparse the ordinary ones instead.
type Zip64Mode int

const (
	// Zip64Auto (the default, and Zip64Mode's zero value) assumes a
	// narrow (32-bit) trailing data descriptor in the ambiguous case —
	// matching every other reader's behavior for a header with no Zip64
	// signal at all. This is wrong for the one narrow scenario
	// archive/zip.Writer itself can produce: a Zip64-sized entry
	// streamed to a non-seekable io.Writer, which never signals Zip64
	// in the local header because the writer discovers the need too
	// late to go back and patch it ("too late anyway", per that
	// package's own writeDataDescriptor comment).
	Zip64Auto Zip64Mode = iota

	// Zip64Force32 states Zip64Auto's ambiguous-case assumption
	// explicitly rather than relying on the implicit default; behavior
	// is identical to Zip64Auto.
	Zip64Force32

	// Zip64Force64 assumes a wide (8-byte) trailing data descriptor in
	// the ambiguous case instead — use this when the archive is known
	// or suspected to come from archive/zip.Writer streaming a
	// Zip64-sized entry to a non-seekable destination.
	Zip64Force64
)

// WithZip64Mode overrides how a Walker resolves the one genuinely
// ambiguous Zip64 scenario described by Zip64Mode. The default,
// Zip64Auto, matches this package's historical (pre-option) behavior
// exactly.
func WithZip64Mode(mode Zip64Mode) Option {
	return func(o *options) {
		o.zip64Mode = mode
	}
}

// parseZip64Extra scans extra (a local file header's raw extra field)
// for a Zip64 extended-information record (zip64ExtraID) and, if
// needUncompressed/needCompressed ask for them, decodes the 64-bit
// sizes it carries -- in that order, the only order a local header's
// Zip64 record ever uses (unlike a central directory entry's, it never
// carries a header offset or disk number). Passing both need flags
// false is a presence check only: sawRecord reports whether a Zip64
// record exists at all, without requiring it to carry any size fields.
func parseZip64Extra(extra []byte, needUncompressed, needCompressed bool) (uncompressed, compressed uint64, sawRecord bool, err error) {
	for len(extra) >= 4 {
		id := binary.LittleEndian.Uint16(extra[0:2])
		size := int(binary.LittleEndian.Uint16(extra[2:4]))

		if len(extra) < 4+size {
			return 0, 0, false, fmt.Errorf("truncated extra field record")
		}

		payload := extra[4 : 4+size]
		extra = extra[4+size:]

		if id != zip64ExtraID {
			continue
		}

		payload, uncompressed, err = takeZip64Field(payload, needUncompressed, "uncompressed size")
		if err != nil {
			return 0, 0, false, err
		}

		_, compressed, err = takeZip64Field(payload, needCompressed, "compressed size")
		if err != nil {
			return 0, 0, false, err
		}

		return uncompressed, compressed, true, nil
	}

	return 0, 0, false, nil
}

// takeZip64Field consumes one 8-byte field from the front of payload
// when want is true, returning the remaining payload and the decoded
// value; when want is false it returns payload untouched and a zero
// value, since that field wasn't flagged as needed in the local header
// and so was never written into the record at all.
func takeZip64Field(payload []byte, want bool, fieldName string) ([]byte, uint64, error) {
	if !want {
		return payload, 0, nil
	}

	if len(payload) < 8 {
		return nil, 0, fmt.Errorf("Zip64 extra field missing %s", fieldName)
	}

	return payload[8:], binary.LittleEndian.Uint64(payload[0:8]), nil
}
