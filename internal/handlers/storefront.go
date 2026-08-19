package handlers

import (
	"context"
	"net/http"
	"time"

	"farmstore/internal/database"
	"farmstore/internal/logutil"
	"farmstore/internal/models"
)

// seasonInfo carries the copy + accent used by the seasonal banner on Home.
// It flips between fig season and pomegranate season through the year.
type seasonInfo struct {
	Key              string // "fig" or "pomegranate"
	Label            string
	Tag              string // small stamp label
	Heading          string // Alyamama headline
	Tagline          string
	Accent           string // underline bar colour
	AccentQuoteColor string // text colour used in the tag stamp
	Image            string
	Target           string // category link for the season's produce
	CTA              string
}

// currentSeason decides the seasonal banner based on the Gregorian month,
// pointing at the fig or pomegranate category depending on the harvest.
func currentSeason() seasonInfo {
	m := time.Now().Month()
	switch {
	case m >= 6 && m <= 8:
		return seasonInfo{
			Key:              "fig",
			Label:            "فصل انجیر",
			Tag:              "ویژه این فصل",
			Heading:          "انجیر خشک، خوشمزه و طبیعی",
			Tagline:          "خشک‌شده زیر آفتاب، بی‌افزودنی.",
			Accent:           "#C98A2C",
			AccentQuoteColor: "#E3B65C",
			Image:            "/assets/fig-showcase.webp",
			Target:           "/products/fig",
			CTA:              "محصولات انجیر را ببین",
		}
	case m >= 9 && m <= 11:
		return seasonInfo{
			Key:              "pomegranate",
			Label:            "فصل انار",
			Tag:              "برداشت پاییز",
			Heading:          "انار تازه، آبدار",
			Tagline:          "از دانه‌ی تازه تا رب و آب‌انار؛ بدون هیچ افزودنی.",
			Accent:           "#C97064",
			AccentQuoteColor: "#D98C80",
			Image:            "/assets/pomegranate-showcase.webp",
			Target:           "/products/pomegranate",
			CTA:              "محصولات انار را ببین",
		}
	default:
		return seasonInfo{
			Key:              "traditional",
			Label:            "محصولات سنتی",
			Tag:              "همیشه موجود",
			Heading:          "محصولات سنتی و خانگی",
			Tagline:          "مربا، رب و ترشی خانگی؛ طعم اصیل باغ.",
			Accent:           "#8C6F5E",
			AccentQuoteColor: "#A68B7B",
			Image:            "/assets/fig-showcase.webp",
			Target:           "/products/traditional",
			CTA:              "محصولات سنتی را ببین",
		}
	}
}

// featuredProducts flattens a small mixed selection of active products from
// the enabled categories for the storefront "منتخب این فصل" row.
func (h *Handler) featuredProducts(ctx context.Context) []models.Product {
	const max = 5
	out := make([]models.Product, 0, max)
	cats, err := database.GetEnabledCategories(ctx, h.db)
	if err != nil {
		return out
	}
	for _, cat := range cats {
		ps, err := database.GetProducts(ctx, h.db, cat.Label)
		if err != nil {
			continue
		}
		for _, p := range ps {
			if len(out) >= max {
				return out
			}
			out = append(out, p)
		}
	}
	return out
}

// Home renders the main storefront page — hero, story strip, featured products,
// and the seasonal banner.
func (h *Handler) Home(w http.ResponseWriter, r *http.Request) {
	data := h.mergeData(r, map[string]any{
		"Featured":      h.featuredProducts(r.Context()),
		"Season":        currentSeason(),
		"CurrentSeason": currentSeason().Key,
	}, w)

	h.render(w, "index", data)
}

// ProductsPage renders the listing for a single category — reusing the same
// product card markup as before. The "all" slug is a special always-available
// filter; any other slug resolves to a seeded category and 404s when missing or
// disabled.
func (h *Handler) ProductsPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("category")

	var category, currentFilter, label string
	if slug == "all" {
		category = "all"
		currentFilter = "all"
		label = "همه محصولات"
	} else {
		cat, err := database.GetCategoryBySlug(r.Context(), h.db, slug)
		if err != nil || !cat.IsEnabled {
			http.NotFound(w, r)
			return
		}
		category = cat.Label
		currentFilter = cat.Slug
		label = cat.Label
	}

	cats, err := database.GetEnabledCategories(r.Context(), h.db)
	if err != nil {
		logutil.Error("enabled categories", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	products, err := database.GetProducts(r.Context(), h.db, category)
	if err != nil {
		logutil.Error("products page", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := h.mergeData(r, map[string]any{
		"Products":      products,
		"Categories":    cats,
		"CurrentFilter": currentFilter,
		"CategoryLabel": label,
	}, w)

	h.render(w, "products", data)
}

// About renders the about-us page with a short introduction to the farm.
func (h *Handler) About(w http.ResponseWriter, r *http.Request) {
	data := h.mergeData(r, nil, w)
	h.render(w, "about", data)
}
