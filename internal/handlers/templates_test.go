package handlers

import (
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTmpl writes a template file and returns its path.
func writeTmpl(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTemplateStoreLoadAndGet(t *testing.T) {
	dir := t.TempDir()
	layout := writeTmpl(t, dir, "layout.html", `{{define "layout.html"}}LAYOUT {{template "content" .}}{{end}}`)
	page := writeTmpl(t, dir, "page.html", `{{define "content"}}HELLO {{.Name}}{{end}}`)

	s := newTemplateStore(template.FuncMap{}, []string{layout}, map[string][]string{"test": {page}}, false)
	if err := s.load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	tpl, err := s.get("test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var b strings.Builder
	if err := tpl.ExecuteTemplate(&b, "layout.html", map[string]string{"Name": "world"}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if b.String() != "LAYOUT HELLO world" {
		t.Fatalf("render = %q", b.String())
	}
}

func TestTemplateStoreMissingPage(t *testing.T) {
	dir := t.TempDir()
	layout := writeTmpl(t, dir, "layout.html", `<html></html>`)

	s := newTemplateStore(template.FuncMap{}, []string{layout}, map[string][]string{"test": {}}, false)
	if err := s.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := s.get("nope"); err == nil {
		t.Fatal("expected error for unknown template name")
	}
}

func TestTemplateStoreDevHotReload(t *testing.T) {
	dir := t.TempDir()
	layout := writeTmpl(t, dir, "layout.html", `{{define "layout.html"}}{{template "content" .}}{{end}}`)
	page := writeTmpl(t, dir, "page.html", `{{define "content"}}V1{{end}}`)

	s := newTemplateStore(template.FuncMap{}, []string{layout}, map[string][]string{"test": {page}}, true)
	if err := s.load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	render := func() string {
		tpl, err := s.get("test")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		var b strings.Builder
		if err := tpl.Execute(&b, nil); err != nil {
			t.Fatalf("execute: %v", err)
		}
		return b.String()
	}

	if got := render(); got != "V1" {
		t.Fatalf("initial render = %q, want %q", got, "V1")
	}

	// Editing the file should be picked up without a rebuild.
	writeTmpl(t, dir, "page.html", `{{define "content"}}V2{{end}}`)
	time.Sleep(10 * time.Millisecond)
	// Force the mtime to differ even on coarse filesystems.
	past := time.Now().Add(-time.Second)
	os.Chtimes(filepath.Join(dir, "page.html"), past, time.Now())
	if got := render(); got != "V2" {
		t.Fatalf("after edit render = %q, want %q", got, "V2")
	}

	// A syntax error during reload must keep serving the last good templates.
	writeTmpl(t, dir, "page.html", `{{define "content"}}BROKEN {{`)
	time.Sleep(10 * time.Millisecond)
	os.Chtimes(filepath.Join(dir, "page.html"), past, time.Now())
	if got := render(); got != "V2" {
		t.Fatalf("after broken edit render = %q, want %q (last known good retained)", got, "V2")
	}

	// Fixing the file reloads the new content.
	writeTmpl(t, dir, "page.html", `{{define "content"}}V3{{end}}`)
	time.Sleep(10 * time.Millisecond)
	os.Chtimes(filepath.Join(dir, "page.html"), time.Now(), time.Now())
	if got := render(); got != "V3" {
		t.Fatalf("after fix render = %q, want %q", got, "V3")
	}
}

func TestTemplateStoreNoReloadOutsideDev(t *testing.T) {
	dir := t.TempDir()
	layout := writeTmpl(t, dir, "layout.html", `{{define "layout.html"}}{{template "content" .}}{{end}}`)
	page := writeTmpl(t, dir, "page.html", `{{define "content"}}V1{{end}}`)

	s := newTemplateStore(template.FuncMap{}, []string{layout}, map[string][]string{"test": {page}}, false)
	if err := s.load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	writeTmpl(t, dir, "page.html", `{{define "content"}}V2{{end}}`)
	time.Sleep(10 * time.Millisecond)

	tpl, err := s.get("test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var b strings.Builder
	if err := tpl.Execute(&b, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if b.String() != "V1" {
		t.Fatalf("non-dev render = %q, want %q", b.String(), "V1")
	}
}

// TestProductsTemplateCategoryBackdrop renders the REAL storefront products
// template and asserts the category image is emitted as a header backdrop
// (with a legibility scrim) only when CategoryImage is set.
func TestProductsTemplateCategoryBackdrop(t *testing.T) {
	layout := filepath.Join("..", "..", "templates", "layout.html")
	page := filepath.Join("..", "..", "templates", "products.html")
	if _, err := os.Stat(layout); err != nil {
		t.Skipf("real templates not available: %v", err)
	}
	tpl, err := template.New("").Funcs(templateFuncs()).ParseFiles(layout, page)
	if err != nil {
		t.Fatalf("parse real templates: %v", err)
	}

	render := func(image string) string {
		var b strings.Builder
		data := map[string]any{
			"CategoryLabel": "انجیر",
			"CurrentFilter": "fig",
			"CategoryImage": image,
			"Products":      nil,
			"Categories":    nil,
		}
		if err := tpl.ExecuteTemplate(&b, "content", data); err != nil {
			t.Fatalf("execute content: %v", err)
		}
		return b.String()
	}

	withImg := render("/uploads/cat.png")
	if !strings.Contains(withImg, "background-image:url('/uploads/cat.png')") {
		t.Fatalf("backdrop image div missing: %s", withImg)
	}
	if !strings.Contains(withImg, "from-sand/95 via-sand/85 to-sand/55") {
		t.Fatal("legibility scrim missing behind category backdrop")
	}

	if withoutImg := render(""); strings.Contains(withoutImg, "background-image:url('/uploads/") ||
		strings.Contains(withoutImg, "from-sand/95") {
		t.Fatal("imageless category must not render a backdrop")
	}
}
