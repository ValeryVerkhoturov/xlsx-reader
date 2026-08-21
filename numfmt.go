package xlsx

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// formatContext holds everything needed to turn a numeric cell's raw
// <v> text into a formatted display string, snapshotted once per Sheet
// (see Reader.NextSheet) from whatever styles/workbook metadata has been
// parsed so far. It is deliberately a snapshot, not a live view: if
// xl/styles.xml is only seen after a worksheet has already been
// returned, that sheet's cells silently stay raw rather than
// retroactively upgrading -- the same graceful-degradation posture as
// Reader.resolveSheet's naming fallback.
type formatContext struct {
	raw      bool           // RawCellValue(true): never format, always return v as-is
	ready    bool           // xl/styles.xml has been parsed
	cellXfs  []int          // style index -> numFmtId
	numFmts  map[int]string // custom numFmtId (>= 164 by convention) -> formatCode
	date1904 bool
}

// formatCellValue returns v formatted per the cell's style (styleIdx,
// its s attribute; "" means style 0, same as an explicit s="0"), or v
// unchanged whenever formatting doesn't apply: raw mode, styles metadata
// unavailable, v isn't a parsable float, or the resolved numFmtId/code
// isn't one this basic engine recognizes.
func (fc *formatContext) formatCellValue(v, styleIdx string) string {
	if fc == nil || fc.raw || !fc.ready {
		return v
	}

	id, ok := fc.lookupNumFmtID(styleIdx)
	if !ok {
		return v
	}

	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return v
	}

	formatted, ok := formatNumeric(id, f, fc.date1904, fc.numFmts)
	if !ok {
		return v
	}

	return formatted
}

func (fc *formatContext) lookupNumFmtID(styleIdx string) (int, bool) {
	idx := 0

	if styleIdx != "" {
		n, err := strconv.Atoi(styleIdx)
		if err != nil || n < 0 {
			return 0, false
		}

		idx = n
	}

	if idx >= len(fc.cellXfs) {
		return 0, false
	}

	return fc.cellXfs[idx], true
}

// formatNumeric formats v per numFmtId id: first against the fixed
// built-in table, then, for a custom id (>= 164), against a date/time
// token translation of its formatCode. ok is false whenever nothing
// recognized applies -- the caller must fall back to raw text.
func formatNumeric(id int, v float64, date1904 bool, custom map[int]string) (string, bool) {
	if s, ok := formatBuiltin(id, v, date1904); ok {
		return s, true
	}

	code, ok := custom[id]
	if !ok {
		return "", false
	}

	tokens, ok := translateCustomDateCode(code)
	if !ok {
		return "", false
	}

	t := timeOfDayFromSerial(v)
	if hasDateToken(tokens) {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return "", false
		}

		t = dateFromSerial(v, date1904)
	}

	return renderDateTokens(tokens, t), true
}

// formatBuiltin handles the fixed set of ECMA-376 built-in numFmtIds
// this "basic" engine recognizes: 1-4 and 9-10 (plain/grouped numbers
// and percentages) and 14-22, 45-47 (the standard date/time formats).
// Everything else, including 0 (General), returns ok=false.
//
// The exact display width/padding chosen for each id (e.g. a
// zero-padded 24-hour "15:04" for ids 20-22 rather than an unpadded
// "15:4") is a best-effort "basic" rendering, not a pixel-perfect
// reproduction of Excel's own locale-dependent output -- callers that
// need that should use RawCellValue and format the serial number
// themselves.
func formatBuiltin(id int, v float64, date1904 bool) (string, bool) {
	switch id {
	case 1:
		return formatFixedGrouped(v, 0, false), true
	case 2:
		return formatFixedGrouped(v, 2, false), true
	case 3:
		return formatFixedGrouped(v, 0, true), true
	case 4:
		return formatFixedGrouped(v, 2, true), true
	case 9:
		return formatFixedGrouped(v*100, 0, false) + "%", true
	case 10:
		return formatFixedGrouped(v*100, 2, false) + "%", true
	}

	isDateTimeID := id >= 14 && id <= 22 || id >= 45 && id <= 47
	if isDateTimeID && (v < 0 || math.IsNaN(v) || math.IsInf(v, 0)) {
		return "", false
	}

	switch id {
	case 14:
		return dateFromSerial(v, date1904).Format("01-02-06"), true
	case 15:
		return dateFromSerial(v, date1904).Format("2-Jan-06"), true
	case 16:
		return dateFromSerial(v, date1904).Format("2-Jan"), true
	case 17:
		return dateFromSerial(v, date1904).Format("Jan-06"), true
	case 18:
		return timeOfDayFromSerial(v).Format("3:04 PM"), true
	case 19:
		return timeOfDayFromSerial(v).Format("3:04:05 PM"), true
	case 20:
		return timeOfDayFromSerial(v).Format("15:04"), true
	case 21:
		return timeOfDayFromSerial(v).Format("15:04:05"), true
	case 22:
		return dateFromSerial(v, date1904).Format("1/2/06 15:04"), true
	case 45:
		return timeOfDayFromSerial(v).Format("04:05"), true
	case 46:
		h, m, s := elapsedHMS(v)
		return fmt.Sprintf("%d:%02d:%02d", h, m, s), true
	case 47:
		return timeOfDayFromSerial(v).Format("04:05.0"), true
	}

	return "", false
}

// groupThousands inserts comma separators into a non-negative decimal
// digit string, e.g. "1234567" -> "1,234,567". digits must contain only
// ASCII '0'-'9' (formatFixedGrouped strips any sign/decimal point
// first).
func groupThousands(digits string) string {
	n := len(digits)
	if n <= 3 {
		return digits
	}

	var b strings.Builder

	first := n % 3
	if first == 0 {
		first = 3
	}

	b.WriteString(digits[:first])

	for i := first; i < n; i += 3 {
		b.WriteByte(',')
		b.WriteString(digits[i : i+3])
	}

	return b.String()
}

// formatFixedGrouped renders v with exactly decimals fractional digits
// (via strconv.FormatFloat's 'f' verb), comma-grouping the integer part
// when group is true.
func formatFixedGrouped(v float64, decimals int, group bool) string {
	s := strconv.FormatFloat(v, 'f', decimals, 64)

	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}

	intPart, fracPart, hasFrac := strings.Cut(s, ".")

	if group {
		intPart = groupThousands(intPart)
	}

	out := intPart
	if hasFrac {
		out += "." + fracPart
	}

	if neg {
		out = "-" + out
	}

	return out
}

// dateFromSerial converts an OOXML date/time serial number to a
// time.Time in UTC, epoch 1899-12-30 (date1904=false) or 1904-01-01
// (date1904=true). The 1900 system's epoch absorbs Excel's fictitious
// Feb-29-1900 leap-year bug correctly for every real date from
// 1900-03-01 onward; serials representing Jan/Feb 1900 are a known,
// accepted inconsistency shared by every implementation of this
// conversion, not specially handled here.
func dateFromSerial(v float64, date1904 bool) time.Time {
	epoch := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	if date1904 {
		epoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	return epoch.Add(time.Duration(math.Round(v*86400)) * time.Second)
}

// timeOfDayFromSerial extracts just v's fractional (time-of-day)
// component as a time.Time on an arbitrary fixed reference date, for
// the pure time-of-day built-ins and non-elapsed custom time codes --
// deliberately not epoch-based, since these never need the date system.
// Unlike dateFromSerial, this keeps sub-second precision (not rounded to
// the nearest whole second), since numFmtId 47 and the custom
// subsecond token both need it.
func timeOfDayFromSerial(v float64) time.Time {
	frac := v - math.Floor(v)

	return time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(frac * 86400 * float64(time.Second)))
}

// elapsedHMS decomposes v's full serial value (not just its fractional
// part) into true elapsed hours/minutes/seconds without wrapping hours
// at 24, for numFmtId 46 ([h]:mm:ss).
func elapsedHMS(v float64) (hours, minutes, seconds int) {
	total := max(int64(math.Round(v*86400)), 0)

	return int(total / 3600), int(total / 60 % 60), int(total % 60)
}

// dateTokenKind classifies one piece of a translated custom format
// code: either a literal run of text to copy verbatim, or a specific
// date/time component to render from a time.Time.
type dateTokenKind int

const (
	tokLiteral dateTokenKind = iota
	tokYear2
	tokYear4
	tokMonthNum
	tokMonthAbbr
	tokMonthFull
	tokDayNum
	tokDayAbbr
	tokDayFull
	tokHour
	tokMinute
	tokSecond
	tokSubSecond
	tokAMPM
)

type dateToken struct {
	kind  dateTokenKind
	width int    // repeated-letter run length (or digit count for tokSubSecond); unused for tokLiteral
	text  string // literal text (tokLiteral), or which AM/PM spelling matched (tokAMPM)
}

// translateCustomDateCode attempts to interpret a custom numFmt's
// formatCode (e.g. `yyyy\-mm\-dd`) as a date/time display format. ok is
// false whenever code contains no recognized date/time token at all
// (not date-like -- the caller should skip formatting entirely, not
// treat it as a translation failure), or contains any construct this
// basic translator can't confidently render: a bracketed section
// ([Red], [h], [$-409], ...), a semicolon-separated multi-section code,
// a numeric-format symbol (# 0 ? @ % $), an unterminated quote or
// trailing backslash, or a letter/run this translator doesn't
// recognize. The caller must fall back to raw text on ok=false, never
// guess.
func translateCustomDateCode(code string) ([]dateToken, bool) {
	runes := []rune(code)
	n := len(runes)

	var tokens []dateToken

	for i := 0; i < n; {
		c := runes[i]

		switch {
		case c == '"':
			j := i + 1
			for j < n && runes[j] != '"' {
				j++
			}

			if j >= n {
				return nil, false
			}

			tokens = append(tokens, dateToken{kind: tokLiteral, text: string(runes[i+1 : j])})
			i = j + 1

		case c == '\\':
			if i+1 >= n {
				return nil, false
			}

			tokens = append(tokens, dateToken{kind: tokLiteral, text: string(runes[i+1])})
			i += 2

		case c == '[' || c == ';' || c == '#' || c == '0' || c == '?' || c == '@' || c == '%' || c == '$':
			return nil, false

		case c == 'A':
			switch {
			case hasRunePrefix(runes[i:], "AM/PM"):
				tokens = append(tokens, dateToken{kind: tokAMPM, text: "AM/PM"})
				i += len("AM/PM")
			case hasRunePrefix(runes[i:], "A/P"):
				tokens = append(tokens, dateToken{kind: tokAMPM, text: "A/P"})
				i += len("A/P")
			default:
				return nil, false
			}

		case isDateLetter(c):
			j := i
			for j < n && runes[j] == c {
				j++
			}

			tok, ok := classifyDateRun(c, j-i)
			if !ok {
				return nil, false
			}

			if tok.kind == tokSecond && j < n && runes[j] == '.' {
				k := j + 1
				for k < n && runes[k] == '0' {
					k++
				}

				if k > j+1 {
					tokens = append(tokens, tok, dateToken{kind: tokSubSecond, width: k - j - 1})
					i = k
					continue
				}
			}

			tokens = append(tokens, tok)
			i = j

		default:
			tokens = append(tokens, dateToken{kind: tokLiteral, text: string(c)})
			i++
		}
	}

	disambiguateMinutes(tokens)

	if !hasNonLiteralToken(tokens) {
		return nil, false
	}

	return tokens, true
}

func hasRunePrefix(s []rune, prefix string) bool {
	pr := []rune(prefix)
	if len(s) < len(pr) {
		return false
	}

	for i, r := range pr {
		if s[i] != r {
			return false
		}
	}

	return true
}

func isDateLetter(c rune) bool {
	return c == 'y' || c == 'm' || c == 'd' || c == 'h' || c == 's'
}

func classifyDateRun(c rune, run int) (dateToken, bool) {
	switch c {
	case 'y':
		switch run {
		case 2:
			return dateToken{kind: tokYear2, width: run}, true
		case 4:
			return dateToken{kind: tokYear4, width: run}, true
		}
	case 'm':
		switch run {
		case 1, 2:
			return dateToken{kind: tokMonthNum, width: run}, true
		case 3:
			return dateToken{kind: tokMonthAbbr, width: run}, true
		case 4:
			return dateToken{kind: tokMonthFull, width: run}, true
		}
	case 'd':
		switch run {
		case 1, 2:
			return dateToken{kind: tokDayNum, width: run}, true
		case 3:
			return dateToken{kind: tokDayAbbr, width: run}, true
		case 4:
			return dateToken{kind: tokDayFull, width: run}, true
		}
	case 'h':
		if run == 1 || run == 2 {
			return dateToken{kind: tokHour, width: run}, true
		}
	case 's':
		if run == 1 || run == 2 {
			return dateToken{kind: tokSecond, width: run}, true
		}
	}

	return dateToken{}, false
}

// disambiguateMinutes reclassifies each tokMonthNum token to tokMinute
// when it's adjacent -- ignoring literal runs -- to an hour token before
// it or a second token after it, matching Excel's own m/mm
// interpretation rule (e.g. "h:mm:ss" -> minutes, "yyyy-mm-dd" -> month).
func disambiguateMinutes(tokens []dateToken) {
	var nonLit []int
	for i, t := range tokens {
		if t.kind != tokLiteral {
			nonLit = append(nonLit, i)
		}
	}

	for pos, idx := range nonLit {
		if tokens[idx].kind != tokMonthNum {
			continue
		}

		var prevKind, nextKind dateTokenKind = -1, -1
		if pos > 0 {
			prevKind = tokens[nonLit[pos-1]].kind
		}
		if pos < len(nonLit)-1 {
			nextKind = tokens[nonLit[pos+1]].kind
		}

		if prevKind == tokHour || nextKind == tokSecond {
			tokens[idx].kind = tokMinute
		}
	}
}

func hasNonLiteralToken(tokens []dateToken) bool {
	for _, t := range tokens {
		if t.kind != tokLiteral {
			return true
		}
	}

	return false
}

func hasDateToken(tokens []dateToken) bool {
	for _, t := range tokens {
		switch t.kind {
		case tokYear2, tokYear4, tokMonthNum, tokMonthAbbr, tokMonthFull, tokDayNum, tokDayAbbr, tokDayFull:
			return true
		}
	}

	return false
}

// renderDateTokens executes a translated token sequence against t,
// concatenating literal runs verbatim and rendering each date/time token
// from t directly (never by building a single composite time.Format
// layout string), so literal text can never be misinterpreted as a
// layout reference number.
func renderDateTokens(tokens []dateToken, t time.Time) string {
	pm := false
	for _, tok := range tokens {
		if tok.kind == tokAMPM {
			pm = true
			break
		}
	}

	var b strings.Builder

	for _, tok := range tokens {
		switch tok.kind {
		case tokLiteral:
			b.WriteString(tok.text)
		case tokYear2:
			b.WriteString(t.Format("06"))
		case tokYear4:
			b.WriteString(t.Format("2006"))
		case tokMonthNum:
			if tok.width == 1 {
				b.WriteString(strconv.Itoa(int(t.Month())))
			} else {
				b.WriteString(t.Format("01"))
			}
		case tokMonthAbbr:
			b.WriteString(t.Format("Jan"))
		case tokMonthFull:
			b.WriteString(t.Format("January"))
		case tokDayNum:
			if tok.width == 1 {
				b.WriteString(strconv.Itoa(t.Day()))
			} else {
				b.WriteString(t.Format("02"))
			}
		case tokDayAbbr:
			b.WriteString(t.Format("Mon"))
		case tokDayFull:
			b.WriteString(t.Format("Monday"))
		case tokHour:
			hour := t.Hour()
			if pm {
				h12 := hour % 12
				if h12 == 0 {
					h12 = 12
				}
				if tok.width == 1 {
					b.WriteString(strconv.Itoa(h12))
				} else {
					fmt.Fprintf(&b, "%02d", h12)
				}
			} else if tok.width == 1 {
				b.WriteString(strconv.Itoa(hour))
			} else {
				fmt.Fprintf(&b, "%02d", hour)
			}
		case tokMinute:
			if tok.width == 1 {
				b.WriteString(strconv.Itoa(t.Minute()))
			} else {
				b.WriteString(t.Format("04"))
			}
		case tokSecond:
			if tok.width == 1 {
				b.WriteString(strconv.Itoa(t.Second()))
			} else {
				b.WriteString(t.Format("05"))
			}
		case tokSubSecond:
			width := tok.width
			if width > 9 {
				width = 9 // nanoseconds can't resolve past 9 digits
			}

			divisor := 1
			for k := 0; k < 9-width; k++ {
				divisor *= 10
			}
			fmt.Fprintf(&b, ".%0*d", width, t.Nanosecond()/divisor)
		case tokAMPM:
			label := "AM"
			if t.Hour() >= 12 {
				label = "PM"
			}
			if tok.text == "A/P" {
				label = label[:1]
			}
			b.WriteString(label)
		}
	}

	return b.String()
}
