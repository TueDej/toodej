package handlers

import (
	"net/http"
	"time"

	"farmstore/internal/database"
	"farmstore/internal/logutil"
	"farmstore/internal/models"
)

// catInfo is the lightweight per-category metadata used to render the home page
// showcase tiles — the Persian label plus a slug used in the URL, a widescreen
// orchard photo for the tile.
type catInfo struct {
	Slug   string
	Label  string
	Image  string // CSS background image URL for the tile
	Season string // season key for matching (spring, summer, autumn, or empty)
	IsSVG  bool   // true if Image is an SVG icon (small centered) vs photo (cover)
}

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

// currentSeason decides the seasonal banner based on the Gregorian month.
func currentSeason() seasonInfo {
	m := time.Now().Month()
	switch {
	case m >= 3 && m <= 5:
		return seasonInfo{
			Key:              "spring",
			Label:            "فصل بهار",
			Tag:              "تازه و سبز",
			Heading:          "محصولات تازه‌ی بهاری",
			Tagline:          "سبزی و میوه‌ی بهاری، تازه و بی‌افزودنی.",
			Accent:           "#3F5D42",
			AccentQuoteColor: "#5A8A60",
			Image:            "/assets/toodej.webp",
			Target:           "/products/spring",
			CTA:              "محصولات بهار را ببین",
		}
	case m >= 6 && m <= 8:
		return seasonInfo{
			Key:              "summer",
			Label:            "فصل تابستان",
			Tag:              "ویژه این فصل",
			Heading:          "انجیر خشک، خوشمزه و طبیعی",
			Tagline:          "خشک‌شده زیر آفتاب، بی‌افزودنی.",
			Accent:           "#C98A2C",
			AccentQuoteColor: "#E3B65C",
			Image:            "/assets/fig-showcase.webp",
			Target:           "/products/summer",
			CTA:              "محصولات این فصل را ببین",
		}
	case m >= 9 && m <= 11:
		return seasonInfo{
			Key:              "autumn",
			Label:            "فصل پاییز",
			Tag:              "برداشت پاییز",
			Heading:          "انار تازه، آبدار",
			Tagline:          "از دانه‌ی تازه تا رب و آب‌انار؛ بدون هیچ افزودنی.",
			Accent:           "#C97064",
			AccentQuoteColor: "#D98C80",
			Image:            "/assets/pomegranate-showcase.webp",
			Target:           "/products/autumn",
			CTA:              "محصولات پاییز را ببین",
		}
	default:
		return seasonInfo{
			Key:              "dried",
			Label:            "خشکبار",
			Tag:              "همیشه موجود",
			Heading:          "خشکبار و آجیل",
			Tagline:          "انجیر خشک، پسته، گردو و بیشتر.",
			Accent:           "#8C6F5E",
			AccentQuoteColor: "#A68B7B",
			Image:            "/assets/fig-showcase.webp",
			Target:           "/products/dried",
			CTA:              "خشکبار را ببین",
		}
	}
}

// featuredProducts flattens a small mixed selection of active products from
// all categories for the storefront "منتخب این فصل" row.
func (h *Handler) featuredProducts() []models.Product {
	const max = 5
	var out []models.Product
	for _, cat := range []string{database.CategorySpring, database.CategorySummer, database.CategoryAutumn, database.CategoryDried, database.CategoryProcessed} {
		ps, err := database.GetProducts(h.db, cat)
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
// seasonal banner, and the five category showcase tiles.
func (h *Handler) Home(w http.ResponseWriter, r *http.Request) {
	cats := []catInfo{
		{
			Slug:   "spring",
			Label:  database.CategorySpring,
			Image:  "/assets/blossoms-and-sky.webp",
			Season: "spring",
		},
		{
			Slug:   "summer",
			Label:  database.CategorySummer,
			Image:  "/assets/summer.webp",
			Season: "summer",
		},
		{
			Slug:   "autumn",
			Label:  database.CategoryAutumn,
			Image:  "/assets/autumn.webp",
			Season: "autumn",
		},
		{
			Slug:  "dried",
			Label: database.CategoryDried,
			Image: "/assets/bowl-nuts.svg?v=3",
			IsSVG: true,
		},
		{
			Slug:  "processed",
			Label: database.CategoryProcessed,
			Image: "/assets/rewilding-meadow.svg?v=3",
			IsSVG: true,
		},
	}

	data := h.mergeData(r, map[string]any{
		"Categories":    cats,
		"Featured":      h.featuredProducts(),
		"Season":        currentSeason(),
		"CurrentSeason": currentSeason().Key,
	}, w)

	h.render(w, "index", data)
}

// ProductsPage renders the listing for a single category — reusing the same
// product card markup as before.
func (h *Handler) ProductsPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("category")

	var category, currentFilter, label string
	switch slug {
	case "spring":
		category = database.CategorySpring
		currentFilter = "spring"
		label = database.CategorySpring
	case "summer":
		category = database.CategorySummer
		currentFilter = "summer"
		label = database.CategorySummer
	case "autumn":
		category = database.CategoryAutumn
		currentFilter = "autumn"
		label = database.CategoryAutumn
	case "dried":
		category = database.CategoryDried
		currentFilter = "dried"
		label = database.CategoryDried
	case "processed":
		category = database.CategoryProcessed
		currentFilter = "processed"
		label = database.CategoryProcessed
	case "all":
		category = "all"
		currentFilter = "all"
		label = "همه محصولات"
	default:
		http.NotFound(w, r)
		return
	}

	products, err := database.GetProducts(h.db, category)
	if err != nil {
		logutil.Error("products page", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := h.mergeData(r, map[string]any{
		"Products":      products,
		"CurrentFilter": currentFilter,
		"CategoryLabel": label,
		"CategorySlug":  currentFilter,
	}, w)

	h.render(w, "products", data)
}

// About renders the about-us page with a short introduction to the farm.
func (h *Handler) About(w http.ResponseWriter, r *http.Request) {
	data := h.mergeData(r, nil, w)
	h.render(w, "about", data)
}
