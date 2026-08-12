package logutil

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/fatih/color"
)

// forceColor turns on fatih/color for the package-level colour instances so the
// handler emits ANSI codes even when stdout is not a TTY (as in `go test`).
func forceColor() {
	color.NoColor = false
	for _, c := range []*color.Color{colorError, colorWarn, colorInfo, colorDebug, colorGrey, colorDim} {
		c.EnableColor()
	}
}

func TestColorTextHandlerEmitsANSI(t *testing.T) {
	forceColor()

	var buf bytes.Buffer
	h := newColorTextHandler(&buf, slog.LevelInfo)
	l := slog.New(h)
	l.Info("hello")
	l.Warn("careful")
	l.Error("boom")
	l.Info("http request", "method", "GET", "path", "/", "status", 200)

	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("expected ANSI escape codes in output, got:\n%s", out)
	}
	// Each severity line should carry its level word.
	for _, lvl := range []string{"INFO", "WARN", "ERROR"} {
		if !strings.Contains(out, lvl) {
			t.Fatalf("expected level %q in output, got:\n%s", lvl, out)
		}
	}
	// The http request line is rendered grey (dim), which still uses ANSI.
	if !strings.Contains(out, "http request") {
		t.Fatalf("expected http request line, got:\n%s", out)
	}
}

func TestColorTextHandlerOmitsAboveLevel(t *testing.T) {
	forceColor()

	var buf bytes.Buffer
	h := newColorTextHandler(&buf, slog.LevelWarn)
	l := slog.New(h)
	l.Info("hidden")
	l.Warn("shown")
	if strings.Contains(buf.String(), "hidden") {
		t.Fatalf("info line should be filtered out at warn level")
	}
	if !strings.Contains(buf.String(), "shown") {
		t.Fatalf("warn line should appear")
	}
}
