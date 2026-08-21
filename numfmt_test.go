package xlsx

import (
	"testing"
	"time"
)

func TestFormatBuiltin(t *testing.T) {
	cases := []struct {
		name     string
		id       int
		v        float64
		date1904 bool
		want     string
		wantOk   bool
	}{
		{"id 1 integer", 1, 1234.5, false, "1234", true}, // strconv rounds half-to-even
		{"id 2 two decimals", 2, 1234.5, false, "1234.50", true},
		{"id 3 grouped integer", 3, 1234567, false, "1,234,567", true},
		{"id 4 grouped two decimals", 4, 1234567.891, false, "1,234,567.89", true},
		{"id 3 negative", 3, -1234567, false, "-1,234,567", true},
		{"id 9 percent", 9, 0.4525, false, "45%", true},
		{"id 10 percent two decimals", 10, 0.4525, false, "45.25%", true},
		// 2024-01-15 is serial 45306 in the 1900 date system.
		{"id 14 mm-dd-yy", 14, 45306, false, "01-15-24", true},
		{"id 15 d-mmm-yy", 15, 45306, false, "15-Jan-24", true},
		{"id 16 d-mmm", 16, 45306, false, "15-Jan", true},
		{"id 17 mmm-yy", 17, 45306, false, "Jan-24", true},
		{"id 18 h:mm AM/PM", 18, 0.75, false, "6:00 PM", true},
		{"id 19 h:mm:ss AM/PM", 19, 0.75, false, "6:00:00 PM", true},
		{"id 20 h:mm", 20, 0.75, false, "18:00", true},
		{"id 21 h:mm:ss", 21, 0.5 + 1.0/24 + 2.0/1440 + 3.0/86400, false, "13:02:03", true},
		{"id 22 date+time", 22, 45306.75, false, "1/15/24 18:00", true},
		{"id 45 mm:ss", 45, 90.0 / 86400, false, "01:30", true}, // 90 seconds into the day
		{"id 46 elapsed [h]:mm:ss over 24h", 46, 1.5, false, "36:00:00", true},
		{"id 47 mm:ss.0", 47, 1.5 / 86400, false, "00:01.5", true}, // 1.5 seconds into the day
		{"id 0 General never formatted", 0, 42, false, "", false},
		{"unrecognized id", 11, 42, false, "", false},
		{"negative date rejected", 14, -1, false, "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := formatBuiltin(c.id, c.v, c.date1904)
			if ok != c.wantOk {
				t.Fatalf("formatBuiltin(%d, %v, %v) ok = %v, want %v (got %q)", c.id, c.v, c.date1904, ok, c.wantOk, got)
			}
			if ok && got != c.want {
				t.Errorf("formatBuiltin(%d, %v, %v) = %q, want %q", c.id, c.v, c.date1904, got, c.want)
			}
		})
	}
}

func TestFormatFixedGrouped(t *testing.T) {
	cases := []struct {
		v        float64
		decimals int
		group    bool
		want     string
	}{
		{0, 0, false, "0"},
		{1234567, 0, true, "1,234,567"},
		{1234567, 0, false, "1234567"},
		{-1234.5, 1, true, "-1,234.5"},
		{999, 0, true, "999"},
		{1000, 0, true, "1,000"},
	}

	for _, c := range cases {
		if got := formatFixedGrouped(c.v, c.decimals, c.group); got != c.want {
			t.Errorf("formatFixedGrouped(%v, %d, %v) = %q, want %q", c.v, c.decimals, c.group, got, c.want)
		}
	}
}

func TestGroupThousands(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"1", "1"},
		{"999", "999"},
		{"1000", "1,000"},
		{"1234567", "1,234,567"},
	}

	for _, c := range cases {
		if got := groupThousands(c.in); got != c.want {
			t.Errorf("groupThousands(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDateFromSerial(t *testing.T) {
	got := dateFromSerial(45306, false)
	want := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("dateFromSerial(45306, false) = %v, want %v", got, want)
	}

	got1904 := dateFromSerial(0, true)
	want1904 := time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got1904.Equal(want1904) {
		t.Errorf("dateFromSerial(0, true) = %v, want %v", got1904, want1904)
	}

	// Known, accepted quirk: real Excel treats serial 60 as its own
	// fictitious "Feb 29 1900", but this epoch-trick conversion uses
	// real Gregorian arithmetic from 1899-12-30 with no special-cased
	// leap day, so serial 60 actually lands one real day earlier, on
	// Feb 28 1900 -- documented as an accepted Jan/Feb-1900
	// inconsistency, not a bug. The two representations agree again from
	// serial 61 (1900-03-01) onward, which is what actually matters.
	quirky := dateFromSerial(60, false)
	wantQuirky := time.Date(1900, 2, 28, 0, 0, 0, 0, time.UTC)
	if !quirky.Equal(wantQuirky) {
		t.Errorf("dateFromSerial(60, false) = %v, want %v", quirky, wantQuirky)
	}

	agree := dateFromSerial(61, false)
	wantAgree := time.Date(1900, 3, 1, 0, 0, 0, 0, time.UTC)
	if !agree.Equal(wantAgree) {
		t.Errorf("dateFromSerial(61, false) = %v, want %v", agree, wantAgree)
	}
}

func TestElapsedHMS(t *testing.T) {
	h, m, s := elapsedHMS(1.5) // 36 hours
	if h != 36 || m != 0 || s != 0 {
		t.Errorf("elapsedHMS(1.5) = %d:%d:%d, want 36:0:0", h, m, s)
	}
}

func TestTranslateCustomDateCode_Success(t *testing.T) {
	cases := []struct {
		name string
		code string
		t    time.Time
		want string
	}{
		{"iso date, month via yyyy/dd adjacency", "yyyy-mm-dd", time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC), "2024-01-15"},
		{"escaped literals, matches real LibreOffice output", `yyyy\-mm\-dd`, time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC), "2024-01-15"},
		{"h precedes m -> minutes", "h:mm:ss", time.Date(1, 1, 1, 13, 2, 3, 0, time.UTC), "13:02:03"},
		{"m precedes no h/s -> month", "mm/dd/yyyy", time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC), "01/15/2024"},
		{"hh precedes mm -> minutes", "hh:mm", time.Date(1, 1, 1, 9, 5, 0, 0, time.UTC), "09:05"},
		{"quoted literal", `yyyy-mm-dd"T"hh:mm:ss`, time.Date(2024, 1, 15, 9, 5, 3, 0, time.UTC), "2024-01-15T09:05:03"},
		{"single m/d/yy", "m/d/yy", time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC), "1/15/24"},
		{"subsecond + AM/PM", "h:mm:ss.00 AM/PM", time.Date(1, 1, 1, 13, 2, 3, 500000000, time.UTC), "1:02:03.50 PM"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tokens, ok := translateCustomDateCode(c.code)
			if !ok {
				t.Fatalf("translateCustomDateCode(%q) ok = false, want true", c.code)
			}
			got := renderDateTokens(tokens, c.t)
			if got != c.want {
				t.Errorf("translateCustomDateCode(%q) rendered %q, want %q", c.code, got, c.want)
			}
		})
	}
}

func TestTranslateCustomDateCode_Abort(t *testing.T) {
	cases := []string{
		`[Red]0.00`,
		`yyyy;mm;dd`,
		`YYYY-MM-DD`,
		`yyy-mm-dd`,   // invalid 3-y run
		`mmmmm`,       // invalid 5-m run
		`yyyy-"mm`,    // unterminated quote
		`yyyy-mm-dd\`, // trailing backslash
	}

	for _, code := range cases {
		t.Run(code, func(t *testing.T) {
			if _, ok := translateCustomDateCode(code); ok {
				t.Errorf("translateCustomDateCode(%q) ok = true, want false (abort)", code)
			}
		})
	}
}

func TestTranslateCustomDateCode_NotDateLike(t *testing.T) {
	cases := []string{"General", "@", "0.00", ""}

	for _, code := range cases {
		t.Run(code, func(t *testing.T) {
			if _, ok := translateCustomDateCode(code); ok {
				t.Errorf("translateCustomDateCode(%q) ok = true, want false (not date-like)", code)
			}
		})
	}
}

func TestFormatNumeric_CustomDateCode(t *testing.T) {
	custom := map[int]string{164: "yyyy-mm-dd"}

	got, ok := formatNumeric(164, 45306, false, custom)
	if !ok {
		t.Fatal("formatNumeric with custom date code: ok = false, want true")
	}
	if want := "2024-01-15"; got != want {
		t.Errorf("formatNumeric = %q, want %q", got, want)
	}

	// A custom id with no formatCode entry, or one that isn't date-like,
	// must fall back cleanly.
	if _, ok := formatNumeric(999, 45306, false, custom); ok {
		t.Error("formatNumeric for unknown custom id: ok = true, want false")
	}
}

func TestRenderDateTokens_SubSecondWidthClamped(t *testing.T) {
	// A subsecond run wider than 9 digits must clamp to nanosecond
	// resolution rather than misbehave on a negative 9-width count.
	tokens, ok := translateCustomDateCode("ss.0000000000")
	if !ok {
		t.Fatal("translateCustomDateCode: ok = false, want true")
	}

	tm := time.Date(1, 1, 1, 0, 0, 1, 500000000, time.UTC)
	if got, want := renderDateTokens(tokens, tm), "01.500000000"; got != want {
		t.Errorf("renderDateTokens = %q, want %q", got, want)
	}
}
