package utils

import (
	"strings"
	"testing"
	"time"
)

func TestFormatPersianDate(t *testing.T) {
	// 2023-09-17 corresponds to ۲۶ شهریور ۱۴۰۲ in the Jalali calendar.
	ts := time.Date(2023, 9, 17, 0, 0, 0, 0, time.UTC)
	got := FormatPersianDate(ts)
	if got != "۲۶ شهریور ۱۴۰۲" {
		t.Fatalf("FormatPersianDate(2023-09-17) = %q", got)
	}
	if !strings.Contains(FormatPersianDate(time.Now()), " ") {
		t.Fatal("missing day-month-year parts")
	}
}

func TestFormatPersianDateTime(t *testing.T) {
	ts := time.Date(2023, 9, 17, 14, 35, 0, 0, time.UTC)
	got := FormatPersianDateTime(ts)
	if !strings.Contains(got, "-") {
		t.Fatalf("FormatPersianDateTime = %q, want time part", got)
	}
	if !strings.HasPrefix(got, "۲۶ شهریور ۱۴۰۲") {
		t.Fatalf("FormatPersianDateTime = %q", got)
	}
}

func TestFormatPersianDateSlash(t *testing.T) {
	ts := time.Date(2023, 9, 17, 0, 0, 0, 0, time.UTC)
	if got := FormatPersianDateSlash(ts); got != "۱۴۰۲/۶/۲۶" {
		t.Fatalf("FormatPersianDateSlash(2023-09-17) = %q", got)
	}
}
