// Package utils provides formatting helpers, primarily for Persian (Jalali) date
// conversion and Persian/Arabic digit representation.
package utils

import (
	"strconv"
	"strings"
	"time"

	"github.com/yaa110/go-persian-calendar"
)

// iranZone is the Iran timezone (UTC+03:30). Iran abolished DST in 1401 (2022),
// so it is +03:30 year-round. All customer-facing dates are displayed in this
// zone so submission times show in Tehran time rather than the UTC in which the
// database stores them.
var iranZone = time.FixedZone("Iran", 3*3600+30*60)

// FormatPersianDate converts a time.Time to a Persian date string like
// "۲۵ شهریور ۱۴۰۳" (day, Persian month name, year).
func FormatPersianDate(t time.Time) string {
	pt := ptime.New(t.In(iranZone))
	return toPersianDigits(pt.Day()) + " " + pt.Month().String() + " " + toPersianDigits(pt.Year())
}

// FormatPersianDateTime converts a time.Time to a Persian date-time string like
// "۲۵ شهریور ۱۴۰۳ - ۱۴:۳۰".
func FormatPersianDateTime(t time.Time) string {
	pt := ptime.New(t.In(iranZone))
	return toPersianDigits(pt.Day()) + " " + pt.Month().String() + " " + toPersianDigits(pt.Year()) +
		" - " + toPersianDigits(pt.Hour()) + ":" + toPersianDigits(pt.Minute())
}

// FormatPersianDateSlash converts a time.Time to a slash-separated Persian date
// like "۱۴۰۳/۶/۲۵" (year/month/day).
func FormatPersianDateSlash(t time.Time) string {
	pt := ptime.New(t.In(iranZone))
	return toPersianDigits(pt.Year()) + "/" + toPersianDigits(int(pt.Month())) + "/" + toPersianDigits(pt.Day())
}

// toPersianDigits converts Western digits (0-9) to their Persian/Arabic Unicode
// equivalents (۰-۹). Non-digit runes pass through unchanged.
func toPersianDigits(v int) string {
	s := strconv.Itoa(v)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r - '0' + 0x06F0)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
