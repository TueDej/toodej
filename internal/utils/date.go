package utils

import (
	"strconv"
	"strings"
	"time"

	"github.com/yaa110/go-persian-calendar"
)

func FormatPersianDate(t time.Time) string {
	pt := ptime.New(t)
	return toPersianDigits(pt.Day()) + " " + pt.Month().String() + " " + toPersianDigits(pt.Year())
}

func FormatPersianDateTime(t time.Time) string {
	pt := ptime.New(t)
	return toPersianDigits(pt.Day()) + " " + pt.Month().String() + " " + toPersianDigits(pt.Year()) +
		" - " + toPersianDigits(pt.Hour()) + ":" + toPersianDigits(pt.Minute())
}

func FormatPersianDateSlash(t time.Time) string {
	pt := ptime.New(t)
	return toPersianDigits(pt.Year()) + "/" + toPersianDigits(int(pt.Month())) + "/" + toPersianDigits(pt.Day())
}

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
