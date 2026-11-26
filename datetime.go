package ibgo

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// tzAliases maps legacy zone names TWS uses to IANA names, so parsing works
// on systems whose tz database lacks the "backward" links.
var tzAliases = map[string]string{
	"US/Eastern":  "America/New_York",
	"US/Central":  "America/Chicago",
	"US/Mountain": "America/Denver",
	"US/Pacific":  "America/Los_Angeles",
	"EST":         "America/New_York",
	"EST5EDT":     "America/New_York",
	"CST":         "America/Chicago",
	"CST6CDT":     "America/Chicago",
	"MET":         "Europe/Amsterdam",
	"GB":          "Europe/London",
	"Hongkong":    "Asia/Hong_Kong",
	"Japan":       "Asia/Tokyo",
}

var tzCache sync.Map // name -> *time.Location (nil value = unknown)

func loadLocation(name string) *time.Location {
	if v, ok := tzCache.Load(name); ok {
		loc, _ := v.(*time.Location)
		return loc
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		if alias, ok := tzAliases[name]; ok {
			loc, err = time.LoadLocation(alias)
		}
	}
	if err != nil {
		loc = nil
	}
	tzCache.Store(name, loc)
	return loc
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseIBDatetime parses the date/time formats TWS sends (same rules as
// ib_async's util.parseIBDatetime):
//
//	"20231019"                       date (UTC midnight)
//	"1697722200"                     unix seconds (formatDate=2)
//	"20231019 09:30:00 US/Eastern"   wall time with zone
//	"20231019  09:30:00"             wall time, no zone (two spaces)
//	"2023-10-19 09:30:00.0"          ISO-ish
//
// Unparseable input returns the zero time.
func parseIBDatetime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if len(s) == 8 && allDigits(s) {
		t, err := time.Parse("20060102", s)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	if allDigits(s) {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return time.Time{}
		}
		return time.Unix(n, 0)
	}
	if strings.Count(s, " ") >= 2 && !strings.Contains(s, "  ") {
		parts := strings.SplitN(s, " ", 3)
		wall := parts[0] + " " + parts[1]
		if loc := loadLocation(parts[2]); loc != nil {
			if t, err := time.ParseInLocation("20060102 15:04:05", wall, loc); err == nil {
				return t
			}
		}
		if t, err := time.Parse("20060102 15:04:05", wall); err == nil {
			return t
		}
		return time.Time{}
	}
	ss := strings.ReplaceAll(strings.ReplaceAll(s, " ", ""), "-", "")
	if len(ss) > 16 {
		ss = ss[:16]
	}
	t, err := time.Parse("2006010215:04:05", ss)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseBarDate is kept as an alias for older call sites.
func parseBarDate(s string) time.Time {
	return parseIBDatetime(s)
}
