package handlers

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"farmstore/internal/database"
)

// pngBytes is a minimal valid 1x1 PNG (passes http.DetectContentType).
var pngBytes = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
	0x0D, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
}

// postMultipart uploads a multipart form (mirroring testClient.do's cookie,
// auth, and CSRF handling) and returns the response.
func (c *testClient) postMultipart(path string, fields url.Values, fileField, fileName string, fileBytes []byte) *http.Response {
	c.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, vs := range fields {
		for _, v := range vs {
			if err := w.WriteField(k, v); err != nil {
				c.t.Fatalf("write field: %v", err)
			}
		}
	}
	if fileField != "" {
		fw, err := w.CreateFormFile(fileField, fileName)
		if err != nil {
			c.t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write(fileBytes); err != nil {
			c.t.Fatalf("write file: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		c.t.Fatalf("close multipart writer: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.srv.URL+path, &buf)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if c.csrfToken != "" {
		req.Header.Set(csrfHeaderName, c.csrfToken)
	}
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("POST multipart %s: %v", path, err)
	}
	// Record cookies and body exactly like testClient.do does.
	for _, ck := range resp.Cookies() {
		c.cookies[ck.Name] = ck
	}
	data := make([]byte, 0)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		data = append(data, tmp[:n]...)
		if err != nil {
			break
		}
	}
	resp.Body.Close()
	resp.Body = http.NoBody
	c.lastBody = data
	return resp
}

// TestAdminProductImageGallery end-to-end: upload → gallery shows it and the
// product's legacy image_url follows; move → order changes; remove → gone.
func TestAdminProductImageGallery(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	resp := c.post("/admin/products", url.Values{
		"name": {"انجیر خشک"}, "category": {"انجیر"}, "price": {"250000"},
		"stock_quantity": {"10"}, "unit": {"۱ کیلوگرم"}, "is_active": {"1"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create product = %d", resp.StatusCode)
	}

	products, err := database.GetProducts(context.Background(), h.db, "انجیر")
	if err != nil || len(products) == 0 {
		t.Fatalf("list products: %v (%d)", err, len(products))
	}
	pid := products[0].ID
	pidStr := strconv.FormatInt(pid, 10)
	fields := url.Values{"owner_type": {"product"}, "owner_id": {pidStr}}

	// Upload two PNGs.
	for i, name := range []string{"a.png", "b.png"} {
		resp := c.postMultipart("/admin/images", fields, "file", name, pngBytes)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("upload #%d = %d", i, resp.StatusCode)
		}
	}

	images, err := database.GetImages(context.Background(), h.db, database.ImageOwnerProduct, pid)
	if err != nil || len(images) != 2 {
		t.Fatalf("GetImages after upload = %v, %v", images, err)
	}
	p, _ := database.GetProduct(context.Background(), h.db, pid)
	if p.ImageURL != images[0] {
		t.Fatalf("image_url = %q, want first image %q", p.ImageURL, images[0])
	}

	// The edit modal shows the gallery.
	if resp := c.get("/admin/products/" + pidStr + "/edit"); resp.StatusCode != http.StatusOK {
		t.Fatalf("edit modal = %d", resp.StatusCode)
	}
	if !strings.Contains(c.body(), images[0]) || !strings.Contains(c.body(), images[1]) {
		t.Fatal("edit modal does not show uploaded images")
	}

	// Move the second image in front: visually that is one slot to the
	// right in the RTL gallery (position 0 renders rightmost).
	entries, _ := database.GetImageEntries(context.Background(), h.db, database.ImageOwnerProduct, pid)
	resp = c.postMultipart("/admin/images/"+strconv.FormatInt(entries[1].ID, 10)+"/move",
		url.Values{"owner_type": {"product"}, "owner_id": {pidStr}, "direction": {"right"}}, "", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("move image = %d", resp.StatusCode)
	}
	images, _ = database.GetImages(context.Background(), h.db, database.ImageOwnerProduct, pid)
	if images[0] != entries[1].Path {
		t.Fatalf("order after move = %v", images)
	}

	// Remove the first image; the other becomes first and image_url follows.
	entries, _ = database.GetImageEntries(context.Background(), h.db, database.ImageOwnerProduct, pid)
	resp = c.postMultipart("/admin/images/"+strconv.FormatInt(entries[0].ID, 10)+"/remove", fields, "", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove image = %d", resp.StatusCode)
	}
	images, _ = database.GetImages(context.Background(), h.db, database.ImageOwnerProduct, pid)
	if len(images) != 1 || images[0] != entries[1].Path {
		t.Fatalf("images after remove = %v", images)
	}
	p, _ = database.GetProduct(context.Background(), h.db, pid)
	if p.ImageURL != images[0] {
		t.Fatalf("image_url after remove = %q", p.ImageURL)
	}

	// The uploaded file really exists on disk under the handler's upload dir.
	if _, err := os.Stat(filepath.Join(h.uploadDir, filepath.Base(images[0]))); err != nil {
		t.Fatalf("uploaded file missing on disk: %v", err)
	}
}

// TestAdminImageUploadRejectsNonImages: content sniffing must reject uploads
// that merely claim an image extension, with graceful Persian feedback.
func TestAdminImageUploadRejectsNonImages(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	resp := c.post("/admin/products", url.Values{
		"name": {"انار"}, "category": {"انار"}, "price": {"100000"},
		"stock_quantity": {"5"}, "is_active": {"1"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create product = %d", resp.StatusCode)
	}
	products, err := database.GetProducts(context.Background(), h.db, "انار")
	if err != nil || len(products) == 0 {
		t.Fatalf("list products: %v (%d)", err, len(products))
	}
	pidStr := strconv.FormatInt(products[0].ID, 10)

	resp = c.postMultipart("/admin/images",
		url.Values{"owner_type": {"product"}, "owner_id": {pidStr}},
		"file", "evil.png", []byte("<html>not an image</html>"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload = %d (expected graceful 200 with flash)", resp.StatusCode)
	}
	images, _ := database.GetImages(context.Background(), h.db, database.ImageOwnerProduct, products[0].ID)
	if len(images) != 0 {
		t.Fatalf("non-image stored: %v", images)
	}
	if !strings.Contains(c.body(), "تصویر") {
		t.Fatalf("no Persian feedback in response: %s", c.body())
	}
}

// TestAdminImageGalleryDomWiring pins the client-side wiring of the upload
// UI: the dropzone label must reference the file input via for=, the input
// must live inside the hidden upload form (so this.form resolves), and the
// broken closest('form') pattern must never come back — the form is a
// sibling of the dropzone, not an ancestor, so closest() always returned nil
// and file selection silently did nothing.
func TestAdminImageGalleryDomWiring(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	resp := c.post("/admin/products", url.Values{
		"name": {"گردو"}, "category": {"محصولات سنتی"}, "price": {"500000"}, "is_active": {"1"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create product = %d", resp.StatusCode)
	}
	products, err := database.GetProducts(context.Background(), h.db, "محصولات سنتی")
	if err != nil || len(products) == 0 {
		t.Fatalf("list products: %v (%d)", err, len(products))
	}
	pidStr := strconv.FormatInt(products[0].ID, 10)

	resp = c.get("/admin/products/" + pidStr + "/edit")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit modal = %d", resp.StatusCode)
	}
	body := c.body()

	wantInput := "image-input-product-" + pidStr
	wantForm := "image-form-product-" + pidStr
	for _, want := range []string{
		`for="` + wantInput + `"`,              // dropzone label targets the input
		`id="` + wantInput + `"`,               // input exists with that id
		`id="` + wantForm + `"`,                // upload form exists with that id
		`onchange="this.form.requestSubmit()"`, // picker path submits its own form
		`adminDropImage(event, this)`,          // drop path is wired
		`hx-encoding="multipart/form-data"`,    // HTMX uploads the file
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("gallery wiring missing %q", want)
		}
	}
	if strings.Contains(body, "closest('form')") || strings.Contains(body, `closest("form")`) {
		t.Fatal("fragile closest('form') pattern is back in the gallery markup")
	}

	// The input must be a child of the form: extract the form element's HTML
	// and confirm the input id appears inside it.
	formStart := strings.Index(body, `id="`+wantForm+`"`)
	if formStart == -1 {
		t.Fatal("upload form not found")
	}
	formEnd := strings.Index(body[formStart:], "</form>")
	if formEnd == -1 {
		t.Fatal("upload form not terminated")
	}
	if !strings.Contains(body[formStart:formStart+formEnd], `id="`+wantInput+`"`) {
		t.Fatal("file input is not inside the upload form")
	}
}

// TestAdminImageMoveRTLOrdering pins the *visual* move semantics of the
// gallery: it flows RTL, so position 0 renders rightmost and later positions
// further left. Pressing ← must move a thumbnail visually left (later in
// order), → visually right (earlier), and the disabled states must match the
// visual edges (→ dead at the rightmost image, ← dead at the leftmost).
func TestAdminImageMoveRTLOrdering(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	resp := c.post("/admin/products", url.Values{
		"name": {"بهار نارنج"}, "category": {"محصولات سنتی"}, "price": {"300000"}, "is_active": {"1"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create product = %d", resp.StatusCode)
	}
	products, err := database.GetProducts(context.Background(), h.db, "محصولات سنتی")
	if err != nil || len(products) == 0 {
		t.Fatalf("list products: %v (%d)", err, len(products))
	}
	pidStr := strconv.FormatInt(products[0].ID, 10)
	fields := url.Values{"owner_type": {"product"}, "owner_id": {pidStr}}

	for _, name := range []string{"a.png", "b.png", "c.png"} {
		if resp := c.postMultipart("/admin/images", fields, "file", name, pngBytes); resp.StatusCode != http.StatusOK {
			t.Fatalf("upload %s = %d", name, resp.StatusCode)
		}
	}
	entries, _ := database.GetImageEntries(context.Background(), h.db, database.ImageOwnerProduct, products[0].ID)
	if len(entries) != 3 {
		t.Fatalf("expected 3 images, got %d", len(entries))
	}

	// The rendered gallery must disable exactly one → (on the first/rightmost
	// image) and exactly one ← (on the last/leftmost image).
	resp = c.get("/admin/products/" + pidStr + "/edit")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit modal = %d", resp.StatusCode)
	}
	if got := strings.Count(c.body(), `title="انتقال به راست" disabled`); got != 1 {
		t.Fatalf("→ disabled on %d thumbnails, want exactly 1 (the rightmost image)", got)
	}
	if got := strings.Count(c.body(), `title="انتقال به چپ" disabled`); got != 1 {
		t.Fatalf("← disabled on %d thumbnails, want exactly 1 (the leftmost image)", got)
	}
	firstBlock := c.body()[:strings.Index(c.body(), entries[1].Path)]
	if !strings.Contains(firstBlock, `title="انتقال به راست" disabled`) {
		t.Fatal("→ is not the disabled button on the first (rightmost) image")
	}

	// ← on the first image must move it visually left: it swaps with the
	// next entry, so the stored order becomes [b, a, c].
	move := func(dir, imageID string) *http.Response {
		return c.postMultipart("/admin/images/"+imageID+"/move",
			url.Values{"owner_type": {"product"}, "owner_id": {pidStr}, "direction": {dir}}, "", "", nil)
	}
	if resp := move("left", strconv.FormatInt(entries[0].ID, 10)); resp.StatusCode != http.StatusOK {
		t.Fatalf("move left = %d", resp.StatusCode)
	}
	images, _ := database.GetImages(context.Background(), h.db, database.ImageOwnerProduct, products[0].ID)
	if images[0] != entries[1].Path || images[1] != entries[0].Path || images[2] != entries[2].Path {
		t.Fatalf("order after ← on first image = %v, want [b a c]", images)
	}

	// → on the now-rightmost image (entries[1]) must be a no-op, and so must
	// ← on the now-leftmost image (entries[2]).
	if resp := move("right", strconv.FormatInt(entries[1].ID, 10)); resp.StatusCode != http.StatusOK {
		t.Fatalf("move right = %d", resp.StatusCode)
	}
	if resp := move("left", strconv.FormatInt(entries[2].ID, 10)); resp.StatusCode != http.StatusOK {
		t.Fatalf("move left = %d", resp.StatusCode)
	}
	images, _ = database.GetImages(context.Background(), h.db, database.ImageOwnerProduct, products[0].ID)
	if images[0] != entries[1].Path || images[1] != entries[0].Path || images[2] != entries[2].Path {
		t.Fatalf("edge moves changed the order: %v, want [b a c]", images)
	}
}

// TestAdminCategorySingleImage: a category keeps exactly one image. A second
// upload replaces the first (its file is deleted from disk), the gallery shows
// the single-image caption and a change-image dropzone, and never renders the
// product-only reorder arrows.
func TestAdminCategorySingleImage(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	if resp := c.post("/admin/categories", url.Values{"slug": {"fig-single"}, "label": {"انجیر"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("create category = %d", resp.StatusCode)
	}
	cats, err := database.GetCategories(context.Background(), h.db)
	if err != nil {
		t.Fatalf("list categories: %v", err)
	}
	var cid int64
	for _, cat := range cats {
		if cat.Slug == "fig-single" {
			cid = cat.ID
		}
	}
	if cid == 0 {
		t.Fatal("created category not found")
	}
	cidStr := strconv.FormatInt(cid, 10)
	fields := url.Values{"owner_type": {"category"}, "owner_id": {cidStr}}

	if resp := c.postMultipart("/admin/images", fields, "file", "a.png", pngBytes); resp.StatusCode != http.StatusOK {
		t.Fatalf("upload a = %d", resp.StatusCode)
	}
	images, _ := database.GetImages(context.Background(), h.db, database.ImageOwnerCategory, cid)
	if len(images) != 1 {
		t.Fatalf("category images after first upload = %v, want exactly 1", images)
	}
	firstPath := images[0]

	if resp := c.get("/admin/categories/" + cidStr + "/edit"); resp.StatusCode != http.StatusOK {
		t.Fatalf("edit modal = %d", resp.StatusCode)
	}
	body := c.body()
	if !strings.Contains(body, "فقط یک تصویر") {
		t.Fatalf("category gallery missing single-image caption: %s", body)
	}
	if strings.Contains(body, "انتقال به چپ") || strings.Contains(body, "انتقال به راست") {
		t.Fatal("category gallery must not render reorder arrows")
	}
	if !strings.Contains(body, "تغییر تصویر") {
		t.Fatal("category gallery should offer a change-image dropzone")
	}

	if resp := c.postMultipart("/admin/images", fields, "file", "b.png", pngBytes); resp.StatusCode != http.StatusOK {
		t.Fatalf("upload b = %d", resp.StatusCode)
	}
	images, _ = database.GetImages(context.Background(), h.db, database.ImageOwnerCategory, cid)
	if len(images) != 1 || images[0] == firstPath {
		t.Fatalf("category images after replace = %v, want one new image", images)
	}
	if _, err := os.Stat(filepath.Join(h.uploadDir, filepath.Base(firstPath))); !os.IsNotExist(err) {
		t.Fatalf("replaced category file still on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.uploadDir, filepath.Base(images[0]))); err != nil {
		t.Fatalf("new category file missing: %v", err)
	}
}

// TestCategoryImageBackdropOnStorefront: the products handler resolves the
// category's single image and passes it to the template as CategoryImage only
// when the category exists and has one; imageless categories and the "all"
// listing pass an empty value so the header renders plain. (The backdrop markup
// itself is covered by TestProductsTemplateCategoryBackdrop.)
func TestCategoryImageBackdropOnStorefront(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	ctx := context.Background()
	catID := func(slug string) int64 {
		cats, err := database.GetCategories(ctx, h.db)
		if err != nil {
			t.Fatalf("list categories: %v", err)
		}
		for _, cat := range cats {
			if cat.Slug == slug {
				return cat.ID
			}
		}
		t.Fatalf("category %q not found", slug)
		return 0
	}

	for _, slug := range []string{"fig-bg", "fig-nobg"} {
		if resp := c.post("/admin/categories", url.Values{"slug": {slug}, "label": {"انجیر"}}); resp.StatusCode != http.StatusOK {
			t.Fatalf("create %s = %d", slug, resp.StatusCode)
		}
	}
	if _, err := database.AddImage(ctx, h.db, database.ImageOwnerCategory, catID("fig-bg"), "/uploads/catbg.png"); err != nil {
		t.Fatalf("attach category image: %v", err)
	}

	if resp := c.get("/products/fig-bg"); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /products/fig-bg = %d", resp.StatusCode)
	}
	if !strings.Contains(c.body(), "IMG=/uploads/catbg.png") {
		t.Fatalf("category page did not receive its image: %s", c.body())
	}

	if resp := c.get("/products/fig-nobg"); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /products/fig-nobg = %d", resp.StatusCode)
	}
	if strings.Contains(c.body(), "IMG=/") {
		t.Fatalf("imageless category should pass an empty CategoryImage: %q", c.body())
	}

	if resp := c.get("/products/all"); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /products/all = %d", resp.StatusCode)
	}
	if strings.Contains(c.body(), "IMG=/") {
		t.Fatalf("/products/all must not carry a category image: %s", c.body())
	}
}
