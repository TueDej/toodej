package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"farmstore/internal/models"
)

// createImageOwnerProduct inserts a minimal product row and returns its id.
func createImageOwnerProduct(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	id, err := CreateProduct(context.Background(), db, &models.Product{
		Name: "انجیر", Category: "انجیر", Price: 1000, StockQuantity: 5, IsActive: true,
	})
	if err != nil {
		t.Fatalf("create product: %v", err)
	}
	return id
}

func TestAddImageOrderingAndSync(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	pid := createImageOwnerProduct(t, db)

	for _, want := range []string{"/uploads/a.png", "/uploads/b.jpg", "/uploads/c.webp"} {
		if _, err := AddImage(ctx, db, ImageOwnerProduct, pid, want); err != nil {
			t.Fatalf("AddImage(%q): %v", want, err)
		}
	}

	got, err := GetImages(ctx, db, ImageOwnerProduct, pid)
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if len(got) != 3 || got[0] != "/uploads/a.png" || got[2] != "/uploads/c.webp" {
		t.Fatalf("GetImages = %v, want insertion order", got)
	}

	// products.image_url must mirror the first image for legacy rendering.
	p, err := GetProduct(ctx, db, pid)
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if p.ImageURL != "/uploads/a.png" {
		t.Fatalf("image_url = %q, want first gallery image", p.ImageURL)
	}
}

func TestRemoveImageRenumbersAndResyncs(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	pid := createImageOwnerProduct(t, db)

	var ids []int64
	for _, p := range []string{"/uploads/1.png", "/uploads/2.png", "/uploads/3.png"} {
		id, err := AddImage(ctx, db, ImageOwnerProduct, pid, p)
		if err != nil {
			t.Fatalf("AddImage(%q): %v", p, err)
		}
		ids = append(ids, id)
	}

	// Remove the first image; the rest shift down and image_url follows.
	if path, err := RemoveImage(ctx, db, ImageOwnerProduct, pid, ids[0]); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	} else if path != "/uploads/1.png" {
		t.Fatalf("RemoveImage path = %q, want /uploads/1.png", path)
	}
	got, err := GetImages(ctx, db, ImageOwnerProduct, pid)
	if err != nil {
		t.Fatalf("GetImages: %v", err)
	}
	if len(got) != 2 || got[0] != "/uploads/2.png" || got[1] != "/uploads/3.png" {
		t.Fatalf("GetImages after remove = %v", got)
	}
	p, _ := GetProduct(ctx, db, pid)
	if p.ImageURL != "/uploads/2.png" {
		t.Fatalf("image_url after remove = %q, want /uploads/2.png", p.ImageURL)
	}

	// Removing a foreign/unknown image id must fail clearly.
	if _, err := RemoveImage(ctx, db, ImageOwnerProduct, pid, ids[0]); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("re-remove = %v, want ErrImageNotFound", err)
	}
}

func TestMoveImageReorders(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	pid := createImageOwnerProduct(t, db)

	var ids []int64
	for _, p := range []string{"/uploads/1.png", "/uploads/2.png", "/uploads/3.png"} {
		id, err := AddImage(ctx, db, ImageOwnerProduct, pid, p)
		if err != nil {
			t.Fatalf("AddImage: %v", err)
		}
		ids = append(ids, id)
	}

	// Move the third image to the front (two left steps).
	if err := MoveImage(ctx, db, ImageOwnerProduct, pid, ids[2], -1); err != nil {
		t.Fatalf("MoveImage left: %v", err)
	}
	if err := MoveImage(ctx, db, ImageOwnerProduct, pid, ids[2], -1); err != nil {
		t.Fatalf("MoveImage left: %v", err)
	}
	got, _ := GetImages(ctx, db, ImageOwnerProduct, pid)
	if got[0] != "/uploads/3.png" || got[1] != "/uploads/1.png" || got[2] != "/uploads/2.png" {
		t.Fatalf("GetImages after moves = %v", got)
	}

	// Moving past either end is a no-op, not an error.
	if err := MoveImage(ctx, db, ImageOwnerProduct, pid, ids[2], -1); err != nil {
		t.Fatalf("MoveImage past start: %v", err)
	}
	got, _ = GetImages(ctx, db, ImageOwnerProduct, pid)
	if got[0] != "/uploads/3.png" {
		t.Fatalf("front image changed by out-of-range move: %v", got)
	}
}

func TestAddImageLimitAndOwners(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	pid := createImageOwnerProduct(t, db)

	for i := 0; i < MaxImagesPerOwner; i++ {
		if _, err := AddImage(ctx, db, ImageOwnerProduct, pid, "/uploads/x.png"); err != nil {
			t.Fatalf("AddImage #%d: %v", i, err)
		}
	}
	if _, err := AddImage(ctx, db, ImageOwnerProduct, pid, "/uploads/over.png"); !errors.Is(err, ErrImageLimitReached) {
		t.Fatalf("overflow AddImage = %v, want ErrImageLimitReached", err)
	}

	// Images cannot be attached to a dangling owner id.
	if _, err := AddImage(ctx, db, ImageOwnerProduct, 99999, "/uploads/x.png"); !errors.Is(err, ErrImageOwnerNotFound) {
		t.Fatalf("dangling owner = %v, want ErrImageOwnerNotFound", err)
	}

	// Categories are supported owners too, without touching products.
	cid, err := CreateCategory(ctx, db, "test-fig-category", "انجیر تست", "")
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	if _, err := AddImage(ctx, db, ImageOwnerCategory, cid, "/uploads/cat.png"); err != nil {
		t.Fatalf("category AddImage: %v", err)
	}
	got, _ := GetImages(ctx, db, ImageOwnerCategory, cid)
	if len(got) != 1 || got[0] != "/uploads/cat.png" {
		t.Fatalf("category GetImages = %v", got)
	}
}

// TestCategorySingleImage: categories hold exactly one image. Appending a
// second is refused, and ReplaceImage swaps the single image (reporting the old
// path for file cleanup) rather than growing the gallery.
func TestCategorySingleImage(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cid, err := CreateCategory(ctx, db, "fig-single", "انجیر", "")
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}

	if _, err := AddImage(ctx, db, ImageOwnerCategory, cid, "/uploads/one.png"); err != nil {
		t.Fatalf("first category AddImage: %v", err)
	}
	if _, err := AddImage(ctx, db, ImageOwnerCategory, cid, "/uploads/two.png"); !errors.Is(err, ErrImageLimitReached) {
		t.Fatalf("second category AddImage = %v, want ErrImageLimitReached", err)
	}

	if _, removed, err := ReplaceImage(ctx, db, ImageOwnerCategory, cid, "/uploads/two.png"); err != nil {
		t.Fatalf("ReplaceImage: %v", err)
	} else if len(removed) != 1 || removed[0] != "/uploads/one.png" {
		t.Fatalf("removed = %v, want [/uploads/one.png]", removed)
	}
	if got, _ := GetImages(ctx, db, ImageOwnerCategory, cid); len(got) != 1 || got[0] != "/uploads/two.png" {
		t.Fatalf("category images after replace = %v, want exactly [two]", got)
	}

	// Replacing again drops the previous single image and keeps exactly one.
	if _, removed, err := ReplaceImage(ctx, db, ImageOwnerCategory, cid, "/uploads/three.png"); err != nil {
		t.Fatalf("second ReplaceImage: %v", err)
	} else if len(removed) != 1 || removed[0] != "/uploads/two.png" {
		t.Fatalf("removed = %v, want [/uploads/two.png]", removed)
	}
	if got, _ := GetImages(ctx, db, ImageOwnerCategory, cid); len(got) != 1 || got[0] != "/uploads/three.png" {
		t.Fatalf("category images = %v, want [three]", got)
	}

	// ReplaceImage refuses a dangling owner id.
	if _, _, err := ReplaceImage(ctx, db, ImageOwnerCategory, 99999, "/uploads/x.png"); !errors.Is(err, ErrImageOwnerNotFound) {
		t.Fatalf("dangling ReplaceImage = %v, want ErrImageOwnerNotFound", err)
	}
}
