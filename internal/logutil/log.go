// Package logutil provides a single, uniform structured logger for the whole
// application built on the standard library's log/slog. It supports log levels
// and JSON output so logs can be aggregated (e.g. by systemd + a log shipper).
//
// Configuration via environment variables:
//
//	LOG_LEVEL  one of debug | info | warn | error  (default: info)
//	LOG_FORMAT one of json | text              (default: json; text in DEV_MODE)
//
// A package-level Logger and thin convenience helpers (Debug/Info/Warn/Error/Fatal)
// are provided so every package logs the same way without boilerplate.
package logutil

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
)

// Logger is the shared application logger. It is initialised once from the
// environment via NewFromEnv and can be swapped (e.g. in tests) with Set.
var Logger *slog.Logger = NewFromEnv()

// NewFromEnv builds a logger from LOG_LEVEL / LOG_FORMAT environment variables,
// falling back to JSON output unless DEV_MODE is set (then text, for readability).
func NewFromEnv() *slog.Logger {
	// Honour the de-facto NO_COLOR convention by disabling fatih/color entirely.
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		color.NoColor = true
	}
	return New(Config{
		Level:  strings.ToLower(os.Getenv("LOG_LEVEL")),
		Format: strings.ToLower(os.Getenv("LOG_FORMAT")),
		Dev:    os.Getenv("DEV_MODE") == "true",
	})
}

// Config controls logger behaviour.
type Config struct {
	Level  string // debug | info | warn | error
	Format string // json | text
	Dev    bool
}

// New constructs a structured logger. Unknown level/format values fall back to
// sensible defaults (info / json).
func New(cfg Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}

	format := cfg.Format
	if format == "" {
		if cfg.Dev {
			format = "text"
		} else {
			format = "json"
		}
	}

	var handler slog.Handler
	switch format {
	case "text":
		// The custom handler colours the level and dims the timestamp. Colours
		// are applied via github.com/fatih/color, which automatically disables
		// itself when stdout is not a TTY (e.g. piped to a file or journald) or
		// when NO_COLOR is set. JSON output stays uncoloured for aggregation.
		handler = newColorTextHandler(os.Stdout, level)
	default:
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

// colour instances used by the console (text) handler. github.com/fatih/color
// takes care of TTY detection and the NO_COLOR convention, so these are safe to
// call unconditionally.
var (
	colorError = color.New(color.FgRed).Add(color.Bold)
	colorWarn  = color.New(color.FgYellow)
	colorInfo  = color.New(color.FgGreen)
	colorDebug = color.New(color.FgCyan)
	// colorDim dims the timestamp.
	colorDim = color.New(color.Faint)
)

// levelColor returns the colour instance for the given level.
func levelColor(l slog.Level) *color.Color {
	switch {
	case l >= slog.LevelError:
		return colorError
	case l >= slog.LevelWarn:
		return colorWarn
	case l >= slog.LevelInfo:
		return colorInfo
	default:
		return colorDebug
	}
}

// colorTextHandler is a minimal slog.Handler that renders records as a single
// coloured text line. It is used instead of slog.TextHandler because the stock
// text handler escapes control characters, which would mangle ANSI codes.
type colorTextHandler struct {
	w     io.Writer
	mu    *sync.Mutex
	level slog.Leveler
	attrs []slog.Attr
}

func newColorTextHandler(w io.Writer, level slog.Level) *colorTextHandler {
	return &colorTextHandler{w: w, mu: &sync.Mutex{}, level: level}
}

func (h *colorTextHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= h.level.Level()
}

func (h *colorTextHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder

	b.WriteString(colorDim.Sprint(r.Time.Format("2006-01-02 15:04:05.000")))
	b.WriteByte(' ')

	b.WriteString(levelColor(r.Level).Sprint(r.Level.String()))
	b.WriteByte(' ')

	b.WriteString(r.Message)

	for _, a := range h.attrs {
		h.writeAttr(&b, a, "")
	}
	r.Attrs(func(a slog.Attr) bool {
		h.writeAttr(&b, a, "")
		return true
	})

	line := b.String() + "\n"

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, line)
	return err
}

func (h *colorTextHandler) writeAttr(b *strings.Builder, a slog.Attr, prefix string) {
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		group := a.Key
		if prefix != "" {
			group = prefix + "." + group
		}
		for _, g := range a.Value.Group() {
			h.writeAttr(b, g, group)
		}
		return
	}
	key := a.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	b.WriteByte(' ')
	b.WriteString(key)
	b.WriteByte('=')
	b.WriteString(a.Value.String())
}

func (h *colorTextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &n
}

func (h *colorTextHandler) WithGroup(_ string) slog.Handler {
	n := *h
	return &n
}

// Info logs at info level.
func Info(msg string, args ...any) { Logger.Info(msg, args...) }

// Warn logs at warn level.
func Warn(msg string, args ...any) { Logger.Warn(msg, args...) }

// Error logs at error level.
func Error(msg string, args ...any) { Logger.Error(msg, args...) }

// Fatal logs at error level then exits the process with status 1. Use for
// unrecoverable startup failures.
func Fatal(msg string, args ...any) {
	Logger.Error(msg, args...)
	os.Exit(1)
}

// statusRecorder captures the HTTP status code and written byte count so the
// access logger can report them.
type statusRecorder struct {
	http.ResponseWriter
	status int
	size   int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.size += n
	return n, err
}

// RequestLogger returns an http middleware that emits one structured access
// log record per request (method, path, status, bytes, duration, remote).
// It is middleware-agnostic and works with chi's r.Use.
func RequestLogger() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			Logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.size,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		})
	}
}
