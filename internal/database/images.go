package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ── Images ───────────────────────────────────────────
//
// Images are managed gallery attachments for products (shown on the storefront
// and in the cart, slidable when there are several) and categories (a single
// image each, stored for future use). A single table serves both owners;
// position keeps the display order contiguous (0..n-1) and products.image_url
// always mirrors the first product image so legacy storefront rendering keeps
// working.

const (
	ImageOwnerProduct  = "product"
	ImageOwnerCategory = "category"
)

// MaxImagesPerOwner caps the gallery size for a product. Categories are limited
// to a single image (see MaxImagesForOwner).
const MaxImagesPerOwner = 10

// MaxImagesForOwner returns how many images an owner type may hold: a full
// gallery for products, exactly one for categories.
func MaxImagesForOwner(ownerType string) int {
	if ownerType == ImageOwnerCategory {
		return 1
	}
	return MaxImagesPerOwner
}

var (
	// ErrImageOwnerNotFound is returned when the owner (product/category) of
	// an image operation does not exist.
	ErrImageOwnerNotFound = errors.New("image owner not found")
	// ErrImageNotFound is returned when a specific image row does not exist
	// for the given owner.
	ErrImageNotFound = errors.New("image not found")
	// ErrImageLimitReached is returned when adding an image beyond the cap.
	ErrImageLimitReached = errors.New("image limit reached")
)

// validImageOwner reports whether ownerType is a known image owner kind.
func validImageOwner(ownerType string) bool {
	return ownerType == ImageOwnerProduct || ownerType == ImageOwnerCategory
}

// GetImages returns the stored image paths for an owner, in display order.
// An owner with no images yields a nil slice.
func GetImages(ctx context.Context, db *sql.DB, ownerType string, ownerID int64) ([]string, error) {
	if !validImageOwner(ownerType) {
		return nil, fmt.Errorf("unknown image owner type %q", ownerType)
	}
	rows, err := db.QueryContext(ctx,
		"SELECT path FROM images WHERE owner_type = ? AND owner_id = ? ORDER BY position, id",
		ownerType, ownerID)
	if err != nil {
		return nil, fmt.Errorf("query images for %s %d: %w", ownerType, ownerID, err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan image path: %w", err)
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// ownerImageExists verifies that the owner row (product or category) exists,
// so images can never be attached to a dangling id.
func ownerImageExists(ctx context.Context, db *sql.DB, ownerType string, ownerID int64) (bool, error) {
	var table string
	switch ownerType {
	case ImageOwnerProduct:
		table = "products"
	case ImageOwnerCategory:
		table = "categories"
	default:
		return false, fmt.Errorf("unknown image owner type %q", ownerType)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE id = ?", ownerID).Scan(&n); err != nil {
		return false, fmt.Errorf("check image owner %s %d: %w", ownerType, ownerID, err)
	}
	return n > 0, nil
}

// AddImage appends a stored image path to an owner's gallery (position: last)
// and, for products, re-syncs products.image_url to the first image.
func AddImage(ctx context.Context, db *sql.DB, ownerType string, ownerID int64, path string) (int64, error) {
	if !validImageOwner(ownerType) {
		return 0, fmt.Errorf("unknown image owner type %q", ownerType)
	}
	if ok, err := ownerImageExists(ctx, db, ownerType, ownerID); err != nil {
		return 0, err
	} else if !ok {
		return 0, fmt.Errorf("%w: %s %d", ErrImageOwnerNotFound, ownerType, ownerID)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin add image tx: %w", err)
	}
	defer tx.Rollback()

	var count int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM images WHERE owner_type = ? AND owner_id = ?", ownerType, ownerID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count images: %w", err)
	}
	if count >= MaxImagesForOwner(ownerType) {
		return 0, fmt.Errorf("%w: %s %d already has %d images", ErrImageLimitReached, ownerType, ownerID, count)
	}

	var position int
	if err := tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(position), -1) FROM images WHERE owner_type = ? AND owner_id = ?", ownerType, ownerID).Scan(&position); err != nil {
		return 0, fmt.Errorf("next image position: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		"INSERT INTO images (owner_type, owner_id, path, position) VALUES (?, ?, ?, ?)",
		ownerType, ownerID, path, position+1)
	if err != nil {
		return 0, fmt.Errorf("insert image: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("image id: %w", err)
	}

	if ownerType == ImageOwnerProduct {
		if err := syncProductImageURLTx(ctx, tx, ownerID); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// ReplaceImage sets a single-image owner's gallery to exactly one image,
// deleting any existing rows and returning their paths so the caller can remove
// the orphaned files from disk. Intended for categories, which hold one image;
// uploading a new one swaps it in rather than erroring at the cap.
func ReplaceImage(ctx context.Context, db *sql.DB, ownerType string, ownerID int64, path string) (int64, []string, error) {
	if !validImageOwner(ownerType) {
		return 0, nil, fmt.Errorf("unknown image owner type %q", ownerType)
	}
	if ok, err := ownerImageExists(ctx, db, ownerType, ownerID); err != nil {
		return 0, nil, err
	} else if !ok {
		return 0, nil, fmt.Errorf("%w: %s %d", ErrImageOwnerNotFound, ownerType, ownerID)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("begin replace image tx: %w", err)
	}
	defer tx.Rollback()

	var removed []string
	rows, err := tx.QueryContext(ctx,
		"SELECT path FROM images WHERE owner_type = ? AND owner_id = ?", ownerType, ownerID)
	if err != nil {
		return 0, nil, fmt.Errorf("list images to replace: %w", err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return 0, nil, fmt.Errorf("scan image path: %w", err)
		}
		removed = append(removed, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, nil, fmt.Errorf("iterate images to replace: %w", err)
	}
	rows.Close()

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM images WHERE owner_type = ? AND owner_id = ?", ownerType, ownerID); err != nil {
		return 0, nil, fmt.Errorf("clear images for %s %d: %w", ownerType, ownerID, err)
	}

	res, err := tx.ExecContext(ctx,
		"INSERT INTO images (owner_type, owner_id, path, position) VALUES (?, ?, ?, 0)",
		ownerType, ownerID, path)
	if err != nil {
		return 0, nil, fmt.Errorf("insert image: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, nil, fmt.Errorf("image id: %w", err)
	}
	return id, removed, tx.Commit()
}

// RemoveImage deletes one image from an owner's gallery, renumbers the
// remaining positions, and re-syncs the product's first-image URL. It returns
// the removed file's stored path so the caller can delete it from disk —
// without this the bytes would accumulate on disk forever after every
// gallery removal.
func RemoveImage(ctx context.Context, db *sql.DB, ownerType string, ownerID, imageID int64) (string, error) {
	if !validImageOwner(ownerType) {
		return "", fmt.Errorf("unknown image owner type %q", ownerType)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin remove image tx: %w", err)
	}
	defer tx.Rollback()

	var path string
	if err := tx.QueryRowContext(ctx,
		"SELECT path FROM images WHERE id = ? AND owner_type = ? AND owner_id = ?", imageID, ownerType, ownerID).Scan(&path); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("%w: image %d for %s %d", ErrImageNotFound, imageID, ownerType, ownerID)
		}
		return "", fmt.Errorf("lookup image %d: %w", imageID, err)
	}
	res, err := tx.ExecContext(ctx,
		"DELETE FROM images WHERE id = ? AND owner_type = ? AND owner_id = ?", imageID, ownerType, ownerID)
	if err != nil {
		return "", fmt.Errorf("delete image %d: %w", imageID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("%w: image %d for %s %d", ErrImageNotFound, imageID, ownerType, ownerID)
	}
	if err := renumberImagesTx(ctx, tx, ownerType, ownerID); err != nil {
		return "", err
	}
	if ownerType == ImageOwnerProduct {
		if err := syncProductImageURLTx(ctx, tx, ownerID); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return path, nil
}

// MoveImage shifts an image one slot left (delta < 0) or right (delta > 0)
// within its gallery. Moving past either end is a no-op, not an error.
func MoveImage(ctx context.Context, db *sql.DB, ownerType string, ownerID, imageID int64, delta int) error {
	if !validImageOwner(ownerType) {
		return fmt.Errorf("unknown image owner type %q", ownerType)
	}
	if delta != -1 && delta != 1 {
		return fmt.Errorf("invalid move delta %d", delta)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin move image tx: %w", err)
	}
	defer tx.Rollback()

	ids, err := ownerImageIDsTx(ctx, tx, ownerType, ownerID)
	if err != nil {
		return err
	}

	idx := -1
	for i, id := range ids {
		if id == imageID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("%w: image %d for %s %d", ErrImageNotFound, imageID, ownerType, ownerID)
	}

	neighbor := idx + delta
	if neighbor >= 0 && neighbor < len(ids) {
		// Swap the two rows' positions, then renumber to keep 0..n-1.
		if _, err := tx.ExecContext(ctx,
			"UPDATE images SET position = CASE id WHEN ? THEN ? WHEN ? THEN ? END WHERE id IN (?, ?)",
			ids[idx], neighbor, ids[neighbor], idx, ids[idx], ids[neighbor]); err != nil {
			return fmt.Errorf("swap image positions: %w", err)
		}
		if err := renumberImagesTx(ctx, tx, ownerType, ownerID); err != nil {
			return err
		}
	}
	if ownerType == ImageOwnerProduct {
		if err := syncProductImageURLTx(ctx, tx, ownerID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ownerImageIDsTx lists an owner's image ids in display order. Callers must
// hold an open transaction.
func ownerImageIDsTx(ctx context.Context, tx *sql.Tx, ownerType string, ownerID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT id FROM images WHERE owner_type = ? AND owner_id = ? ORDER BY position, id", ownerType, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan image id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// renumberImagesTx rewrites positions as 0..n-1 following the current
// (position, id) order. Callers must hold an open transaction.
func renumberImagesTx(ctx context.Context, tx *sql.Tx, ownerType string, ownerID int64) error {
	ids, err := ownerImageIDsTx(ctx, tx, ownerType, ownerID)
	if err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, "UPDATE images SET position = ? WHERE id = ?", i, id); err != nil {
			return fmt.Errorf("renumber image %d: %w", id, err)
		}
	}
	return nil
}

// syncProductImageURLTx points products.image_url at the first gallery image
// (or clears it) so legacy storefront/cart rendering keeps working without
// knowing about the gallery table. Callers must hold an open transaction.
func syncProductImageURLTx(ctx context.Context, tx *sql.Tx, productID int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE products SET image_url = COALESCE(
			(SELECT path FROM images WHERE owner_type = 'product' AND owner_id = ? ORDER BY position, id LIMIT 1), '')
			WHERE id = ?`, productID, productID); err != nil {
		return fmt.Errorf("sync product %d image_url: %w", productID, err)
	}
	return nil
}

// ImageEntry pairs an image row id with its stored path, for UIs that need to
// reference individual images (remove/move buttons).
type ImageEntry struct {
	ID   int64
	Path string
}

// GetImageEntries returns an owner's gallery rows (id + path) in display order.
func GetImageEntries(ctx context.Context, db *sql.DB, ownerType string, ownerID int64) ([]ImageEntry, error) {
	if !validImageOwner(ownerType) {
		return nil, fmt.Errorf("unknown image owner type %q", ownerType)
	}
	rows, err := db.QueryContext(ctx,
		"SELECT id, path FROM images WHERE owner_type = ? AND owner_id = ? ORDER BY position, id",
		ownerType, ownerID)
	if err != nil {
		return nil, fmt.Errorf("query image entries for %s %d: %w", ownerType, ownerID, err)
	}
	defer rows.Close()

	var entries []ImageEntry
	for rows.Next() {
		var e ImageEntry
		if err := rows.Scan(&e.ID, &e.Path); err != nil {
			return nil, fmt.Errorf("scan image entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
