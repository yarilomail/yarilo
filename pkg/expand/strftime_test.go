package expand

import (
	"regexp"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// strftime against the C library's own output in the POSIX locale, for a
// morning, an afternoon and a midnight at the turn of an ISO year: the
// conversions that differ between them are the ones a port gets wrong.
func TestStrftimeMatchesTheCLibrary(t *testing.T) {
	morning := time.Unix(1749379200, 0).UTC()   // Sun 2025-06-08 10:40:00
	afternoon := time.Unix(1749395225, 0).UTC() // Sun 2025-06-08 15:07:05
	midnight := time.Unix(1735517109, 0).UTC()  // Mon 2024-12-30 00:05:09, ISO week 1 of 2025
	tests := []struct {
		at     time.Time
		format string
		want   string
	}{
		{morning, "%a %A %b %h %B", "Sun Sunday Jun Jun June"},
		{morning, "%c", "Sun Jun  8 10:40:00 2025"},
		{morning, "%C %d %D %e %F", "20 08 06/08/25  8 2025-06-08"},
		{morning, "%g %G %H %I %j %k %l", "25 2025 10 10 159 10 10"},
		{morning, "%m %M%n%p %r %R", "06 40\nAM 10:40:00 AM 10:40"},
		{morning, "%s %S%t%T", "1749379200 00\t10:40:00"},
		{morning, "%u %U %V %w %W", "7 23 23 0 22"},
		{morning, "%x %X %y %Y %z %Z %%", "06/08/25 10:40:00 25 2025 +0000 UTC %"},
		{afternoon, "%I %l %p %P %r %k", "03  3 PM pm 03:07:05 PM 15"},
		{midnight, "%a %b %c", "Mon Dec Mon Dec 30 00:05:09 2024"},
		{midnight, "%g %G %V %U %W %u %w", "25 2025 01 52 53 1 1"},
		{midnight, "%H %I %k %l %p %P %j", "00 12  0 12 AM am 365"},
		{midnight, "%e %d %y", "30 30 24"},
		// An unknown conversion is kept as written, and a lone % at the end
		// is literal.
		{morning, "%Q and 100%", "%Q and 100%"},
	}
	for _, tc := range tests {
		if got := strftime(tc.format, tc.at); got != tc.want {
			t.Errorf("strftime(%q, %s) = %q, want %q", tc.format, tc.at, got, tc.want)
		}
	}
}

// lenumber and benumber read the value's bytes as an unsigned integer of
// that width, and refuse a width there is no integer for.
func TestNumberFilters(t *testing.T) {
	p := &Params{Table: map[string]string{
		"one": "ff", "two": "0102", "four": "01020304", "eight": "0102030405060708", "three": "010203",
	}}
	runCases(t, p, []expandCase{
		ok("%{one | unhexlify | lenumber}", "255"),
		ok("%{two | unhexlify | lenumber}", "513"),
		ok("%{two | unhexlify | benumber}", "258"),
		ok("%{four | unhexlify | lenumber}", "67305985"),
		ok("%{four | unhexlify | benumber}", "16909060"),
		ok("%{eight | unhexlify | benumber}", "72623859790382856"),
		ok("%{eight | unhexlify | lenumber}", "578437695752307201"),
		fails("%{three | unhexlify | lenumber}", "Cannot convert '3' bytes to number"),
		fails("%{two | unhexlify | lenumber(1)}", "lenumber"),
		fails("%{lenumber}", "convert to number"),
	})
}

// list joins tab-separated values and undoes the \x01 escape inside each, so
// a value that held a tab, a newline or the escape byte itself reads back.
func TestListUnescapesEachValue(t *testing.T) {
	p := &Params{Table: map[string]string{
		"v":    "a\x01tb\tc\x01nd\te\x01rf\tg\x011h\tz\x010y\tlast\x01",
		"pure": "one\ttwo",
	}}
	runCases(t, p, []expandCase{
		ok("%{v | list('|')}", "a\tb|c\nd|e\rf|g\x01h|z\x00y|last\x01"),
		ok("%{pure | list}", "one,two"),
		ok("%{pure | list(' ')}", "one two"),
	})
}

// The date, time and system providers answer from the clock and the host.
func TestClockAndHostProviders(t *testing.T) {
	before := time.Now()
	checks := []struct {
		key string
		re  string
	}{
		{"date:year", `^\d{4}$`},
		{"date:month", `^(0[1-9]|1[0-2])$`},
		{"date:day", `^(0[1-9]|[12]\d|3[01])$`},
		{"time:hour", `^([01]\d|2[0-3])$`},
		{"time:min", `^[0-5]\d$`},
		{"time:minute", `^[0-5]\d$`},
		{"time:sec", `^[0-5]\d$`},
		{"time:second", `^[0-5]\d$`},
		{"time:us", `^\d{1,6}$`},
		{"time:usec", `^\d{1,6}$`},
		{"system:hostname", `.`},
	}
	if runtime.GOOS != "windows" {
		checks = append(checks, struct{ key, re string }{"system:os", `^[A-Za-z]+$`}, struct{ key, re string }{"system:os-version", `.`})
	}
	for _, c := range checks {
		got, err := Expand("%{"+c.key+"}", nil)
		if err != nil || !regexp.MustCompile(c.re).MatchString(got) {
			t.Errorf("%s = %q, %v", c.key, got, err)
		}
	}
	if y, _ := Expand("%{date:year}", nil); y != strconv.Itoa(before.Year()) && y != strconv.Itoa(time.Now().Year()) {
		t.Errorf("date:year = %q, want this year", y)
	}
	t.Setenv("NCPU", "17")
	if got, _ := Expand("%{system:cpu_count}", nil); got != "17" {
		t.Errorf("system:cpu_count with NCPU=17 = %q", got)
	}
	runCases(t, nil, []expandCase{
		fails("%{date:week}", "Unsupported key 'week'"),
		fails("%{time:millis}", "Unsupported key 'millis'"),
		fails("%{system:nope}", "Unsupported field 'nope'"),
		fails("%{env:}", "Missing field"),
	})
	for _, k := range []string{"uid", "gid"} {
		if got, err := Expand("%{process:"+k+"}", nil); err != nil || got == "" {
			t.Errorf("process:%s = %q, %v", k, got, err)
		}
	}
}
