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

// needsWideDescriptor reports whether a streamed entry's true sizes --
// only knowable with certainty once the entry has actually finished
// decompressing -- require the trailing data descriptor's 8-byte Zip64
// size fields. This is used only when the local header gave no Zip64
// signal at all (see Walker.Next's "Zip64" section): it mirrors the
// exact rule the format itself uses for the sentinel field -- a real
// 32-bit size tops out at zip32SizeSentinel-1, so a true size of
// zip32SizeSentinel or more could never have been encoded any other
// way.
func needsWideDescriptor(uncompressedN, compressedN uint64) bool {
	return uncompressedN >= zip32SizeSentinel || compressedN >= zip32SizeSentinel
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
