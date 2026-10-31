package expand

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// parseUnixTime reads "<seconds>[.<fraction>]"; the fraction is cut or padded
// to nanoseconds.
func parseUnixTime(s string) (int64, int64, error) {
	secPart, frac, hasFrac := strings.Cut(s, ".")
	sec, err := strconv.ParseInt(secPart, 10, 64)
	if err != nil || secPart == "" || secPart[0] == '+' {
		return 0, 0, fmt.Errorf("Invalid timestamp '%s'", s)
	}
	var nsec int64
	if hasFrac {
		scale := int64(100000000)
		for _, ch := range frac {
			if ch < '0' || ch > '9' {
				return 0, 0, fmt.Errorf("Invalid timestamp '%s'", s)
			}
			nsec += int64(ch-'0') * scale
			scale /= 10
		}
	}
	return sec, nsec, nil
}

// namedOrAt reads the one unit, format or tz style parameter: by key, or at
// position pos.
func (c *call) namedOrAt(key string, pos int, dst *string) error {
	for i := range c.params() {
		p := &c.params()[i]
		if p.key == key || p.key == "" && p.idx == pos {
			v, err := c.str(p)
			if err != nil {
				return err
			}
			*dst = v
		}
	}
	return nil
}

func (c *call) onlyKeys(keys ...string) error {
	for i := range c.params() {
		p := &c.params()[i]
		if p.key == "" {
			if p.idx >= len(keys) {
				return errTooManyPositional
			}
			continue
		}
		known := false
		for _, k := range keys {
			known = known || k == p.key
		}
		if !known {
			return errUnsupportedKey(p.key)
		}
	}
	return nil
}

func fnEpoch(c *call) error {
	unit := "s"
	if err := c.onlyKeys("unit"); err != nil {
		return err
	}
	if err := c.namedOrAt("unit", 0, &unit); err != nil {
		return err
	}
	if err := c.needTransfer("convert to epoch"); err != nil {
		return err
	}
	sec, nsec, err := parseUnixTime(string(c.st.transfer))
	if err != nil {
		return err
	}
	var mul, add int64
	switch unit {
	case "s":
		c.st.setString(strconv.FormatInt(sec, 10))
		return nil
	case "ms":
		mul, add = 1000, nsec/1000000
	case "us":
		mul, add = 1000000, nsec/1000
	case "ns":
		mul, add = 1000000000, nsec
	default:
		return fmt.Errorf("Unsupported unit '%s' for 'epoch'", unit)
	}
	if sec > math.MaxInt64/mul || sec < math.MinInt64/mul || sec*mul > math.MaxInt64-add {
		return fmt.Errorf("Timestamp '%s' out of range for unit '%s'", c.st.transfer, unit)
	}
	c.st.setString(strconv.FormatInt(sec*mul+add, 10))
	return nil
}

func fnFromEpoch(c *call) error {
	unit := "s"
	if err := c.onlyKeys("unit"); err != nil {
		return err
	}
	if err := c.namedOrAt("unit", 0, &unit); err != nil {
		return err
	}
	if err := c.needTransfer("convert from epoch"); err != nil {
		return err
	}
	v, err := strconv.ParseInt(string(c.st.transfer), 10, 64)
	if err != nil {
		return fmt.Errorf("Invalid timestamp '%s'", c.st.transfer)
	}
	var div uint64
	switch unit {
	case "s":
		div = 1
	case "ms":
		div = 1000
	case "us":
		div = 1000000
	case "ns":
		div = 1000000000
	default:
		return fmt.Errorf("Unsupported unit '%s' for 'from_epoch'", unit)
	}
	neg := v < 0
	u := uint64(v)
	if neg {
		u = -u
	}
	sign := ""
	if neg {
		sign = "-"
	}
	c.st.setString(fmt.Sprintf("%s%d.%09d", sign, u/div, (u%div)*(1000000000/div)))
	return nil
}

// zone is the timezone parameter: utc (or gmt) or local.
func zone(name, filter string) (*time.Location, error) {
	switch name {
	case "utc", "gmt":
		return time.UTC, nil
	case "local":
		return time.Local, nil
	}
	return nil, fmt.Errorf("Unsupported timezone '%s' for '%s'", name, filter)
}

func fnDate(c *call) error {
	var format *string
	tz := "utc"
	if err := c.onlyKeys("format", "tz"); err != nil {
		return err
	}
	var f string
	for i := range c.params() {
		p := &c.params()[i]
		if p.key == "format" || p.key == "" && p.idx == 0 {
			v, err := c.str(p)
			if err != nil {
				return err
			}
			f = v
			format = &f
		}
	}
	if err := c.namedOrAt("tz", 1, &tz); err != nil {
		return err
	}
	if format == nil {
		return errors.New("Missing date format")
	}
	if err := c.needTransfer("format as date"); err != nil {
		return err
	}
	sec, _, err := parseUnixTime(string(c.st.transfer))
	if err != nil {
		return err
	}
	loc, err := zone(tz, "date")
	if err != nil {
		return err
	}
	c.st.setString(strftime(*format, time.Unix(sec, 0).In(loc)))
	return nil
}

func fnISO8601(c *call) error {
	tz := "utc"
	if err := c.onlyKeys("tz"); err != nil {
		return err
	}
	if err := c.namedOrAt("tz", 0, &tz); err != nil {
		return err
	}
	if err := c.needTransfer("format as ISO 8601"); err != nil {
		return err
	}
	sec, _, err := parseUnixTime(string(c.st.transfer))
	if err != nil {
		return err
	}
	loc, err := zone(tz, "iso8601")
	if err != nil {
		return err
	}
	t := time.Unix(sec, 0).In(loc)
	if loc == time.UTC {
		c.st.setString(t.Format("2006-01-02T15:04:05Z"))
	} else {
		c.st.setString(t.Format("2006-01-02T15:04:05-07:00"))
	}
	return nil
}

var (
	weekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	months   = []string{"January", "February", "March", "April", "May", "June", "July",
		"August", "September", "October", "November", "December"}
)

// strftime formats t with the C library's conversions in the POSIX locale.
func strftime(format string, t time.Time) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			b.WriteByte(format[i])
			continue
		}
		i++
		switch format[i] {
		case 'a':
			b.WriteString(weekdays[t.Weekday()][:3])
		case 'A':
			b.WriteString(weekdays[t.Weekday()])
		case 'b', 'h':
			b.WriteString(months[t.Month()-1][:3])
		case 'B':
			b.WriteString(months[t.Month()-1])
		case 'c':
			b.WriteString(strftime("%a %b %e %H:%M:%S %Y", t))
		case 'C':
			fmt.Fprintf(&b, "%02d", t.Year()/100)
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'D':
			b.WriteString(strftime("%m/%d/%y", t))
		case 'e':
			fmt.Fprintf(&b, "%2d", t.Day())
		case 'F':
			b.WriteString(strftime("%Y-%m-%d", t))
		case 'g':
			y, _ := t.ISOWeek()
			fmt.Fprintf(&b, "%02d", y%100)
		case 'G':
			y, _ := t.ISOWeek()
			fmt.Fprintf(&b, "%d", y)
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'I':
			fmt.Fprintf(&b, "%02d", hour12(t))
		case 'j':
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case 'k':
			fmt.Fprintf(&b, "%2d", t.Hour())
		case 'l':
			fmt.Fprintf(&b, "%2d", hour12(t))
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'n':
			b.WriteByte('\n')
		case 'p':
			if t.Hour() < 12 {
				b.WriteString("AM")
			} else {
				b.WriteString("PM")
			}
		case 'P':
			if t.Hour() < 12 {
				b.WriteString("am")
			} else {
				b.WriteString("pm")
			}
		case 'r':
			b.WriteString(strftime("%I:%M:%S %p", t))
		case 'R':
			b.WriteString(strftime("%H:%M", t))
		case 's':
			fmt.Fprintf(&b, "%d", t.Unix())
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case 't':
			b.WriteByte('\t')
		case 'T':
			b.WriteString(strftime("%H:%M:%S", t))
		case 'u':
			wd := int(t.Weekday())
			if wd == 0 {
				wd = 7
			}
			fmt.Fprintf(&b, "%d", wd)
		case 'U':
			fmt.Fprintf(&b, "%02d", (t.YearDay()+6-int(t.Weekday()))/7)
		case 'V':
			_, w := t.ISOWeek()
			fmt.Fprintf(&b, "%02d", w)
		case 'w':
			fmt.Fprintf(&b, "%d", int(t.Weekday()))
		case 'W':
			fmt.Fprintf(&b, "%02d", (t.YearDay()+6-(int(t.Weekday())+6)%7)/7)
		case 'x':
			b.WriteString(strftime("%m/%d/%y", t))
		case 'X':
			b.WriteString(strftime("%H:%M:%S", t))
		case 'y':
			fmt.Fprintf(&b, "%02d", t.Year()%100)
		case 'Y':
			fmt.Fprintf(&b, "%d", t.Year())
		case 'z':
			b.WriteString(t.Format("-0700"))
		case 'Z':
			b.WriteString(t.Format("MST"))
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(format[i])
		}
	}
	return b.String()
}

func hour12(t time.Time) int {
	h := t.Hour() % 12
	if h == 0 {
		h = 12
	}
	return h
}
