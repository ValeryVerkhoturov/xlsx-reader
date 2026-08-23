package zipstream

// Tests in this file run the walker against ZIP bytes it doesn't control
// the shape of: real archives produced by third-party tools (Info-ZIP's
// zip, Python's zipfile module -- see testdata/generate.sh) and by
// archive/zip.Writer, Go's own independent implementation. This
// complements walker_test.go/zip64_test.go/decompressor_test.go, whose
// fixtures are all hand-built via buildLocalEntry/assembleLocalEntry: those
// pin exact documented behavior for specific byte-level scenarios, but
// can't catch a case where the walker happens to agree with the test
// author's assumptions about ZIP's shape rather than with a real writer's.
// archive/zip.OpenReader/NewReader -- which reads the central directory,
// a wholly different code path from this package's forward-only local-
// entry walk -- serves as an independent oracle for the entries' expected
// names and content wherever a fixture's content isn't already known
// up front.

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// referenceEntries opens path with the standard library's own archive/zip
// (central-directory-based, entirely independent of this package's
// forward-only local-entry walk) and returns each entry's name and
// decompressed content, in central-directory order -- which real writers
// (including both fixtures this reads) write in the same order as the
// local entries, so it doubles as the order this package's Walker should
// produce.
func referenceEntries(t *testing.T, path string) (names []string, contents [][]byte) {
	t.Helper()

	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("archive/zip.OpenReader(%q): %v", path, err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("archive/zip: opening %q: %v", f.Name, err)
		}

		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("archive/zip: reading %q: %v", f.Name, err)
		}

		names = append(names, f.Name)
		contents = append(contents, data)
	}

	return names, contents
}

// walkAll drives w to exhaustion, returning each entry's name and content
// in the order Next produced them.
func walkAll(t *testing.T, w *Walker) (names []string, contents [][]byte) {
	t.Helper()

	for {
		name, entry, err := w.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if entry == nil {
			return names, contents
		}

		data, err := io.ReadAll(entry)
		if err != nil {
			t.Fatalf("reading entry %q: %v", name, err)
		}

		if err := entry.Finish(); err != nil {
			t.Fatalf("Finish %q: %v", name, err)
		}

		names = append(names, name)
		contents = append(contents, data)
	}
}

// TestWalker_RealFixtures walks the checked-in store.zip and deflate.zip
// fixtures (see testdata/generate.sh) -- real archives from Info-ZIP's zip,
// each with a multi-file, subdirectory-path, zero-length-entry mix -- and
// checks the walker reproduces exactly what archive/zip's independent
// central-directory reader reports for the same file.
func TestWalker_RealFixtures(t *testing.T) {
	for _, fixture := range []string{"store.zip", "deflate.zip"} {
		t.Run(fixture, func(t *testing.T) {
			path := filepath.Join("testdata", fixture)

			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open %q: %v", path, err)
			}
			defer f.Close()

			wantNames, wantContents := referenceEntries(t, path)

			gotNames, gotContents := walkAll(t, New(f))

			if len(gotNames) != len(wantNames) {
				t.Fatalf("got %d entries, want %d (got names %v, want %v)", len(gotNames), len(wantNames), gotNames, wantNames)
			}

			for i := range wantNames {
				if gotNames[i] != wantNames[i] {
					t.Errorf("entry %d: name = %q, want %q", i, gotNames[i], wantNames[i])
				}
				if !bytes.Equal(gotContents[i], wantContents[i]) {
					t.Errorf("entry %d (%q): content = %q, want %q", i, wantNames[i], gotContents[i], wantContents[i])
				}
			}
		})
	}
}

// TestWalker_RealFixture_Zip64Signaled walks the checked-in zip64.zip
// fixture -- a single entry Python's zipfile module (force_zip64=True)
// wrote with genuine sentinel sizes and a matching Zip64 extra-field
// record, despite being far too small for any writer to need Zip64 on its
// own merits. Unlike TestWalker_Zip64NonStreaming in zip64_test.go, which
// pins the same scenario from hand-built bytes, this proves the walker
// agrees with a second, real, independent implementation's actual framing
// choices, not just this package's own assumptions about them.
func TestWalker_RealFixture_Zip64Signaled(t *testing.T) {
	path := filepath.Join("testdata", "zip64.zip")

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %q: %v", path, err)
	}
	defer f.Close()

	wantNames, wantContents := referenceEntries(t, path)
	if len(wantNames) != 1 {
		t.Fatalf("fixture has %d entries, want 1", len(wantNames))
	}

	name, entry, err := New(f).Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if entry == nil {
		t.Fatal("Next: entry = nil, want the fixture's one entry")
	}
	if name != wantNames[0] {
		t.Fatalf("name = %q, want %q", name, wantNames[0])
	}

	got, err := io.ReadAll(entry)
	if err != nil {
		t.Fatalf("reading entry: %v", err)
	}
	if err := entry.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if !bytes.Equal(got, wantContents[0]) {
		t.Fatalf("content = %q, want %q", got, wantContents[0])
	}
}

// TestWalker_RealStreamingFromArchiveZipWriter builds a multi-entry ZIP in
// memory using archive/zip.Writer -- Go's own independent implementation,
// and the one this package's doc comments discuss by name. CreateHeader
// always defers sizes to a trailing data descriptor (general-purpose bit
// 3) regardless of whether the destination is seekable, since it never
// reads back to patch the local header -- confirmed by reading
// archive/zip/writer.go's own writeHeader/writeDataDescriptor -- so this
// exercises the walker's streaming path (which walker_test.go otherwise
// only reaches via hand-built buildLocalEntry fixtures) against a real
// writer's genuine streamed output.
func TestWalker_RealStreamingFromArchiveZipWriter(t *testing.T) {
	entries := []struct {
		name string
		data []byte
	}{
		{"first.xml", []byte("first entry's content, written through archive/zip.Writer")},
		{"dir/second.xml", []byte("second entry, nested under a directory path")},
		{"empty.xml", nil},
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	for _, e := range entries {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: e.name, Method: zip.Deflate})
		if err != nil {
			t.Fatalf("CreateHeader(%q): %v", e.name, err)
		}
		if _, err := fw.Write(e.data); err != nil {
			t.Fatalf("writing %q: %v", e.name, err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("closing archive/zip.Writer: %v", err)
	}

	// Confirm the assumption the test's doc comment relies on: every
	// entry archive/zip.Writer just produced is in fact streamed.
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("archive/zip.NewReader: %v", err)
	}
	for _, f := range zr.File {
		if f.Flags&0x8 == 0 {
			t.Fatalf("entry %q: general-purpose flag = %#x, want bit 3 (streamed) set", f.Name, f.Flags)
		}
	}

	gotNames, gotContents := walkAll(t, New(bytes.NewReader(buf.Bytes())))

	if len(gotNames) != len(entries) {
		t.Fatalf("got %d entries, want %d", len(gotNames), len(entries))
	}

	for i, e := range entries {
		if gotNames[i] != e.name {
			t.Errorf("entry %d: name = %q, want %q", i, gotNames[i], e.name)
		}
		if !bytes.Equal(gotContents[i], e.data) {
			t.Errorf("entry %d (%q): content = %q, want %q", i, e.name, gotContents[i], e.data)
		}
	}
}
