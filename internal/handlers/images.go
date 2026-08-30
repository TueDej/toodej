package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"farmstore/internal/database"
	"farmstore/internal/logutil"
)

// ── Image uploads (admin) ────────────────────────────
//
// Admins attach images to products (storefront + cart, slidable) and
// categories (stored for future use). Uploads are multipart POSTs to
// /admin/images; files are validated by content sniffing (not just extension),
// capped at 5 MiB, and stored under a server-generated random name so neither
// the filename nor the extension is attacker-controlled.

const (
	maxImageUploadBytes = 5 << 20 // 5 MiB per image
	maxImageBodyBytes   = 8 << 20 // multipart overhead headroom
)

// allowedImageExts maps sniffed content types to file extensions.
var allowedImageExts = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// parseImageOwner validates and parses the owner_type/owner_id form fields
// shared by all image endpoints.
func parseImageOwner(r *http.Request) (string, int64, error) {
	ownerType := r.FormValue("owner_type")
	if ownerType != database.ImageOwnerProduct && ownerType != database.ImageOwnerCategory {
		return "", 0, errors.New("invalid owner_type")
	}
	ownerID, err := strconv.ParseInt(r.FormValue("owner_id"), 10, 64)
	if err != nil || ownerID <= 0 {
		return "", 0, errors.New("invalid owner_id")
	}
	return ownerType, ownerID, nil
}

// saveUploadedImage validates and stores one uploaded file, returning its
// public path ("/uploads/<random>.<ext>").
func (h *Handler) saveUploadedImage(fh *multipart.FileHeader) (string, error) {
	if fh.Size > maxImageUploadBytes {
		return "", errors.New("فایل بزرگ‌تر از ۵ مگابایت است")
	}
	f, err := fh.Open()
	if err != nil {
		return "", fmt.Errorf("open upload: %w", err)
	}
	defer f.Close()

	// Sniff the real content type from the leading bytes; a .png-named file
	// that is actually HTML (or anything else) is rejected here.
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", fmt.Errorf("read upload head: %w", err)
	}
	ext, ok := allowedImageExts[http.DetectContentType(head[:n])]
	if !ok {
		return "", errors.New("فرمت تصویر پشتیبانی نمی‌شود؛ فقط JPG، PNG، WebP یا GIF")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind upload: %w", err)
	}

	name := make([]byte, 16)
	if _, err := rand.Read(name); err != nil {
		return "", fmt.Errorf("generate image name: %w", err)
	}
	path := filepath.Join(h.uploadDir, hex.EncodeToString(name)+ext)
	if err := os.MkdirAll(h.uploadDir, 0o755); err != nil {
		return "", fmt.Errorf("create upload dir: %w", err)
	}
	dst, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("create image file: %w", err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, f); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("write image file: %w", err)
	}
	return "/uploads/" + filepath.Base(path), nil
}

// AdminUploadImage stores an uploaded image against a product or category and
// re-renders the gallery fragment. The response is always 200 (with a flash
// message in the fragment) so HTMX can swap it even on validation failures.
func (h *Handler) AdminUploadImage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImageBodyBytes)
	if err := r.ParseMultipartForm(maxImageBodyBytes); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ownerType, ownerID, err := parseImageOwner(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	flash := ""
	if fh := r.MultipartForm.File["file"]; len(fh) > 0 {
		path, err := h.saveUploadedImage(fh[0])
		if err != nil {
			flash = err.Error()
		} else {
			// Categories hold a single image: a new upload replaces (and its
			// file deletes) whatever was there. Products append to the gallery.
			var addErr error
			if ownerType == database.ImageOwnerCategory {
				var removed []string
				_, removed, addErr = database.ReplaceImage(r.Context(), h.db, ownerType, ownerID, path)
				for _, old := range removed {
					os.Remove(filepath.Join(h.uploadDir, filepath.Base(old)))
				}
			} else {
				_, addErr = database.AddImage(r.Context(), h.db, ownerType, ownerID, path)
			}
			switch {
			case addErr == nil:
				if ownerType == database.ImageOwnerCategory {
					flash = "تصویر دسته‌بندی ذخیره شد."
				} else {
					flash = "تصویر اضافه شد."
				}
			case errors.Is(addErr, database.ErrImageOwnerNotFound):
				os.Remove(filepath.Join(h.uploadDir, filepath.Base(path)))
				http.Error(w, "owner not found", http.StatusNotFound)
				return
			case errors.Is(addErr, database.ErrImageLimitReached):
				os.Remove(filepath.Join(h.uploadDir, filepath.Base(path)))
				flash = "حداکثر تعداد تصاویر پر است."
			default:
				os.Remove(filepath.Join(h.uploadDir, filepath.Base(path)))
				logutil.Error("add image", "err", addErr)
				flash = "ذخیره‌ی تصویر ناموفق بود."
			}
		}
	} else {
		flash = "فایلی انتخاب نشده است."
	}

	h.renderImageGallery(w, r, ownerType, ownerID, flash)
}

// AdminRemoveImage deletes one gallery image and re-renders the fragment.
func (h *Handler) AdminRemoveImage(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ownerType, ownerID, err := parseImageOwner(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	imageID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid image id", http.StatusBadRequest)
		return
	}

	flash := ""
	if err := database.RemoveImage(r.Context(), h.db, ownerType, ownerID, imageID); err != nil {
		if errors.Is(err, database.ErrImageNotFound) {
			flash = "تصویر یافت نشد."
		} else {
			logutil.Error("remove image", "err", err)
			flash = "حذف تصویر ناموفق بود."
		}
	} else {
		flash = "تصویر حذف شد."
	}
	h.renderImageGallery(w, r, ownerType, ownerID, flash)
}

// AdminMoveImage shifts one gallery image a slot left or right (RTL display)
// and re-renders the fragment.
func (h *Handler) AdminMoveImage(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ownerType, ownerID, err := parseImageOwner(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	imageID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid image id", http.StatusBadRequest)
		return
	}
	var delta int
	switch r.FormValue("direction") {
	case "left":
		// Visual direction, not list direction: the gallery flows RTL, so
		// position 0 renders rightmost and higher positions render further
		// left. Moving a thumbnail visually left therefore means moving it
		// later in display order (+1).
		delta = 1
	case "right":
		delta = -1
	default:
		http.Error(w, "invalid direction", http.StatusBadRequest)
		return
	}

	flash := ""
	if err := database.MoveImage(r.Context(), h.db, ownerType, ownerID, imageID, delta); err != nil {
		if errors.Is(err, database.ErrImageNotFound) {
			flash = "تصویر یافت نشد."
		} else {
			logutil.Error("move image", "err", err)
			flash = "جابه‌جایی تصویر ناموفق بود."
		}
	}
	h.renderImageGallery(w, r, ownerType, ownerID, flash)
}

// renderImageGallery writes the image manager fragment for an owner to w.
func (h *Handler) renderImageGallery(w http.ResponseWriter, r *http.Request, ownerType string, ownerID int64, flash string) {
	html, err := h.renderImageGalleryString(r, ownerType, ownerID, flash)
	if err != nil {
		logutil.Error("render image gallery", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
}

// renderImageGalleryString returns the image manager fragment shown inside
// the product/category edit modal: thumbnails with remove/reorder controls
// plus a drop zone that doubles as a file picker. The upload form is separate
// and hidden; picking a file (or dropping one) submits it via HTMX multipart.
func (h *Handler) renderImageGalleryString(r *http.Request, ownerType string, ownerID int64, flash string) (string, error) {
	entries, err := database.GetImageEntries(r.Context(), h.db, ownerType, ownerID)
	if err != nil {
		return "", fmt.Errorf("list images for %s %d: %w", ownerType, ownerID, err)
	}

	ownerLabel := "محصول"
	singleImage := ownerType == database.ImageOwnerCategory
	if singleImage {
		ownerLabel = "دسته‌بندی"
	}
	caption := fmt.Sprintf("تصاویر %s (حداکثر %s تصویر؛ تصویر اول، تصویر اصلی است)",
		ownerLabel, toPersianDigits(strconv.Itoa(database.MaxImagesForOwner(ownerType))))
	if singleImage {
		caption = "تصویر دسته‌بندی (فقط یک تصویر)"
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<div id="gallery-%s-%d" class="mt-2">
  <p class="mb-2 text-xs text-clay">%s</p>
  <div class="flex flex-wrap gap-3">`, ownerType, ownerID, caption)

	for i, e := range entries {
		hxVals := fmt.Sprintf(`{"owner_type":"%s","owner_id":"%d"`, ownerType, ownerID)

		// Reorder arrows are meaningless for a single-image owner (categories),
		// so they render only for product galleries.
		nav := ""
		if !singleImage {
			leftDisabled, rightDisabled := "", ""
			// Disabled states follow the visual RTL layout: position 0 renders
			// rightmost, so the → button is a no-op there; the last entry renders
			// leftmost, so ← is a no-op there.
			if i == 0 {
				rightDisabled = "disabled opacity-30"
			}
			if i == len(entries)-1 {
				leftDisabled = "disabled opacity-30"
			}
			nav = fmt.Sprintf(`
      <div class="mt-1 flex justify-center gap-1" dir="ltr">
        <button type="button" title="انتقال به چپ" %s
          hx-post="/admin/images/%d/move" hx-vals='%s,"direction":"left"}' hx-target="#gallery-%s-%d" hx-swap="outerHTML"
          class="flex h-6 w-6 items-center justify-center rounded-full border border-line text-xs text-clay transition hover:border-fig hover:text-fig">←</button>
        <button type="button" title="انتقال به راست" %s
          hx-post="/admin/images/%d/move" hx-vals='%s,"direction":"right"}' hx-target="#gallery-%s-%d" hx-swap="outerHTML"
          class="flex h-6 w-6 items-center justify-center rounded-full border border-line text-xs text-clay transition hover:border-fig hover:text-fig">→</button>
      </div>`,
				leftDisabled, e.ID, hxVals, ownerType, ownerID,
				rightDisabled, e.ID, hxVals, ownerType, ownerID)
		}

		fmt.Fprintf(&b, `
    <div class="relative">
      <img src="%s" alt="" class="h-20 w-20 rounded-xl border border-line object-cover">
      <button type="button" title="حذف تصویر"
        hx-post="/admin/images/%d/remove" hx-vals='%s}' hx-target="#gallery-%s-%d" hx-swap="outerHTML"
        class="absolute -top-2 -left-2 flex h-6 w-6 items-center justify-center rounded-full bg-pomegranate text-xs font-bold text-parchment shadow transition hover:opacity-80">×</button>%s
    </div>`,
			e.Path, e.ID, hxVals, ownerType, ownerID, nav)
	}

	addLabel := "افزودن تصویر"
	if singleImage && len(entries) > 0 {
		addLabel = "تغییر تصویر"
	}
	fmt.Fprintf(&b, `
    <label for="image-input-%s-%d" class="image-dropzone flex h-20 w-20 cursor-pointer flex-col items-center justify-center gap-1 rounded-xl border-2 border-dashed border-line text-center text-[10px] leading-4 text-clay transition hover:border-saffron hover:text-fig"
      ondragover="event.preventDefault(); this.classList.add('border-saffron','text-fig')"
      ondragleave="this.classList.remove('border-saffron','text-fig')"
      ondrop="event.preventDefault(); this.classList.remove('border-saffron','text-fig'); adminDropImage(event, this)">
      <span class="text-lg leading-none">＋</span>
      <span>%s</span>
    </label>
  </div>`, ownerType, ownerID, addLabel)

	if flash != "" {
		fmt.Fprintf(&b, `<p class="mt-2 text-xs font-medium text-fig" role="status">%s</p>`, flash)
	}

	fmt.Fprintf(&b, `
  <form id="image-form-%s-%d" hx-post="/admin/images" hx-encoding="multipart/form-data" hx-target="#gallery-%s-%d" hx-swap="outerHTML" class="hidden">
    <input type="hidden" name="owner_type" value="%s">
    <input type="hidden" name="owner_id" value="%d">
    <input type="hidden" name="csrf_token" value="%s">
    <input type="file" id="image-input-%s-%d" name="file" accept="image/jpeg,image/png,image/webp,image/gif" class="hidden"
      onchange="this.form.requestSubmit()">
  </form>
</div>`, ownerType, ownerID, ownerType, ownerID, ownerType, ownerID, ensureCSRFToken(nil, r), ownerType, ownerID)

	return b.String(), nil
}
