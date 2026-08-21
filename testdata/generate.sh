#!/bin/sh
# Regenerates the real-world (non-self-produced) .xlsx fixtures used by
# reader_test.go, via LibreOffice headless.
# Run from the repo root: sh testdata/generate.sh
set -eu

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Mixed types with text -- exercises real shared-strings usage.
cat >"$tmp/mixed.csv" <<'EOF'
name,age,active,score,note
Alice,30,TRUE,95.5,hello
Bob,25,FALSE,88,"multi
line"
Carol,,TRUE,,unicode: héllo 世界
EOF

# Numbers only, no text anywhere (including the header row) -- LibreOffice
# then omits xl/sharedStrings.xml entirely, and blank cells are dropped
# from the row rather than written empty, producing real sparse rows.
cat >"$tmp/numeric.csv" <<'EOF'
1,2,3,4
5,,7,
9,10,,12
EOF

# ISO-looking dates -- LibreOffice's CSV importer auto-detects these as
# dates and assigns a *custom* numFmt (id >= 164, not one of the fixed
# built-in ids), with each literal hyphen backslash-escaped
# (formatCode="yyyy\-mm\-dd"). Exercises the custom date-code translator
# against a real writer's escaping convention, not just hand-built input.
cat >"$tmp/dates.csv" <<'EOF'
2024-01-15
2023-12-25
EOF

soffice --headless --convert-to xlsx --outdir "$tmp" "$tmp/mixed.csv"
soffice --headless --convert-to xlsx --outdir "$tmp" "$tmp/numeric.csv"
soffice --headless --convert-to xlsx --outdir "$tmp" "$tmp/dates.csv"

cp "$tmp/mixed.xlsx" testdata/libreoffice_mixed.xlsx
cp "$tmp/numeric.xlsx" testdata/libreoffice_numeric.xlsx
cp "$tmp/dates.xlsx" testdata/libreoffice_dates.xlsx
