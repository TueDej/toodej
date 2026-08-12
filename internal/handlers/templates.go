package handlers

import (
	"fmt"
	"html/template"
	"net/http"
	"os"
	"sync"
	"time"

	"farmstore/internal/logutil"
)

// TemplateStore owns the parsed HTML templates and supports dev-mode hot
// reload. Templates are re-parsed from disk whenever any underlying file's
// modification time changes (dev mode only). If a re-parse fails, the
// previously parsed (last known good) templates are kept and the server keeps
// serving them, so a syntax error while editing no longer crashes the process
// or requires a restart.
type TemplateStore struct {
	funcMap     template.FuncMap
	layoutFiles []string
	pages       map[string][]string
	devMode     bool

	mu       sync.RWMutex
	cache    map[string]*template.Template
	modTimes map[string]time.Time // file path → mtime observed at last successful parse
}

// newTemplateStore constructs a TemplateStore. devMode enables hot reload and
// lenient startup behaviour.
func newTemplateStore(funcMap template.FuncMap, layoutFiles []string, pages map[string][]string, devMode bool) *TemplateStore {
	return &TemplateStore{
		funcMap:     funcMap,
		layoutFiles: layoutFiles,
		pages:       pages,
		devMode:     devMode,
		cache:       make(map[string]*template.Template),
		modTimes:    make(map[string]time.Time),
	}
}

// load parses every configured page from disk into a fresh cache. On any parse
// error the current cache is left untouched (last known good is preserved).
func (s *TemplateStore) load() error {
	fresh := make(map[string]*template.Template, len(s.pages))
	modTimes := make(map[string]time.Time)
	for name, files := range s.pages {
		all := make([]string, 0, len(s.layoutFiles)+len(files))
		all = append(all, s.layoutFiles...)
		all = append(all, files...)
		t, err := template.New("layout.html").Funcs(s.funcMap).ParseFiles(all...)
		if err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
		fresh[name] = t
		for _, f := range all {
			st, err := os.Stat(f)
			if err != nil {
				return fmt.Errorf("stat %s: %w", f, err)
			}
			modTimes[f] = st.ModTime()
		}
	}

	s.mu.Lock()
	s.cache = fresh
	s.modTimes = modTimes
	s.mu.Unlock()
	return nil
}

// refresh re-parses templates in dev mode if any template file changed on disk.
// A failed re-parse is logged and the previous templates are kept.
func (s *TemplateStore) refresh() {
	if !s.devMode {
		return
	}

	changed := false
	s.mu.RLock()
	for _, files := range s.pages {
		for _, f := range append(append([]string{}, s.layoutFiles...), files...) {
			st, err := os.Stat(f)
			if err != nil {
				s.mu.RUnlock()
				logutil.Error("template refresh: stat failed, keeping previous templates", "file", f, "err", err)
				return
			}
			if prev, ok := s.modTimes[f]; !ok || !prev.Equal(st.ModTime()) {
				changed = true
			}
		}
	}
	s.mu.RUnlock()

	if !changed {
		return
	}
	if err := s.load(); err != nil {
		logutil.Error("template reload failed, keeping previous templates", "err", err)
		return
	}
	logutil.Info("templates reloaded from disk (dev hot reload)")
}

// get returns the cached template for the named page, refreshing first in dev
// mode. It returns an error if the template is not available.
func (s *TemplateStore) get(name string) (*template.Template, error) {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.cache[name]
	if !ok {
		return nil, fmt.Errorf("template %q not available", name)
	}
	return t, nil
}

// render executes the named page template (wrapping layout.html + page file)
// writing to w, refreshing templates from disk first in dev mode.
func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	t, err := h.templates.get(name)
	if err != nil {
		logutil.Error("render failed", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := t.Execute(w, data); err != nil {
		logutil.Error("render failed", "page", name, "err", err)
	}
}

// renderTemplate executes a named sub-template (partial) from the page template.
func (h *Handler) renderTemplate(w http.ResponseWriter, name, tplName string, data any) {
	t, err := h.templates.get(name)
	if err != nil {
		logutil.Error("render partial failed", "page", name, "partial", tplName, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(w, tplName, data); err != nil {
		logutil.Error("render partial failed", "page", name, "partial", tplName, "err", err)
	}
}
