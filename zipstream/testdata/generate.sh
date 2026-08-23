#!/bin/sh
# Regenerates the real (non-self-produced) ZIP fixtures used by
# realdata_test.go: archives built by actual third-party tools -- Info-ZIP's
# zip and Python's zipfile module -- rather than this package's own
# hand-rolled byte-level test helpers (buildLocalEntry et al. in
# walker_test.go), so the walker is proven against bytes it doesn't control
# the shape of.
#
# Run from the repo root: sh zipstream/testdata/generate.sh
# Requires "zip" (Info-ZIP) and "python3" on PATH.
set -eu

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir "$tmp/sub"
printf 'hello from a stored entry\n' >"$tmp/a.txt"
# Long enough, and repetitive enough, to actually compress under Deflate --
# a single short line round-trips through Store either way and wouldn't
# tell the two fixtures apart.
yes 'the quick brown fox jumps over the lazy dog, repeated for compressibility' \
  | head -n 20 >"$tmp/b.txt"
printf 'nested file content\n' >"$tmp/sub/c.txt"
: >"$tmp/empty.txt" # zero-length entry: a real edge case, not a hypothetical

# store.zip: every entry Stored (-0), sizes and CRCs known up front in the
# local header -- the common, fully unambiguous non-streaming case, but
# from a real, independent writer.
(cd "$tmp" && zip -q -X -0 store.zip a.txt b.txt sub/c.txt empty.txt)

# deflate.zip: same files, Info-ZIP's default (Deflate for anything that
# shrinks, Store otherwise) -- exercises the real flate.Reader path against
# another implementation's real compressed output.
(cd "$tmp" && zip -q -X deflate.zip a.txt b.txt sub/c.txt empty.txt)

# zip64.zip: Python's zipfile module, force_zip64=True, on a file tiny
# enough that no real writer would ever choose Zip64 for it on its own --
# this is the "some writers always use Zip64 regardless of actual size"
# case walker_test.go's comments call out, but genuinely produced (sentinel
# 0xFFFFFFFF sizes plus a matching extra-field record) by a real,
# independent implementation rather than assembled by hand.
python3 - "$tmp/zip64.zip" <<'EOF'
import sys
import zipfile

with zipfile.ZipFile(sys.argv[1], "w") as zf:
    with zf.open("part.txt", "w", force_zip64=True) as f:
        f.write(b"tiny payload, but forced into real zip64 signaling by python's zipfile module\n")
EOF

cp "$tmp/store.zip" zipstream/testdata/store.zip
cp "$tmp/deflate.zip" zipstream/testdata/deflate.zip
cp "$tmp/zip64.zip" zipstream/testdata/zip64.zip
