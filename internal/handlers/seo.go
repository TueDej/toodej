package handlers

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"farmstore/internal/models"
)

// siteName is the brand used across SEO metadata and structured data.
const siteName = "تودج"

// defaultDescription is the fallback meta description for pages that do not
// supply their own (cart, login, checkout, …).
const defaultDescription = "تودج؛ فروشگاه محصولات طبیعی باغی — انجیر خشک، انار تازه و محصولات سنتی و خانگی، تازه و درجه یک."

// ogImagePath is the site image reused for OpenGraph / Twitter link previews.
// It is an existing bundled asset, so no extra upload is required.
const ogImagePath = "/assets/fig-showcase.webp"

// logoPath is the brand mark used as favicon and the Organization logo.
const logoPath = "/assets/flower-fig-tree.svg"

// absoluteURL resolves a site-relative path against the configured canonical
// base host. Already-absolute URLs are returned unchanged.
func (h *Handler) absoluteURL(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	base := strings.TrimRight(h.baseURL, "/")
	if path == "" {
		return base + "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

// canonicalURL is the request path resolved against the canonical base host,
// with any trailing slash trimmed (except for the site root).
func (h *Handler) canonicalURL(r *http.Request) string {
	p := r.URL.Path
	if p != "/" {
		p = strings.TrimRight(p, "/")
	}
	return h.absoluteURL(p)
}

// seoJSONLD builds a JSON-LD <script> block for the page. It always emits the
// Organization and WebSite nodes; when products are supplied it adds an
// ItemList of Product nodes (price, currency, availability) so Google can show
// rich results, and a BreadcrumbList when the page is a category listing.
//
// The returned value is safe to drop into <head> as template.HTML: Go's
// json.Marshal escapes <, > and & so the payload can never break out of the
// surrounding <script> tag.
func (h *Handler) seoJSONLD(r *http.Request, products []models.Product, categoryLabel, categorySlug string) template.HTML {
	graph := []any{
		map[string]any{
			"@type": "Organization",
			"name":  siteName,
			"url":   h.absoluteURL("/"),
			"logo":  h.absoluteURL(logoPath),
		},
		map[string]any{
			"@type":      "WebSite",
			"name":       siteName,
			"url":        h.absoluteURL("/"),
			"inLanguage": "fa",
		},
	}

	if categorySlug != "" {
		graph = append(graph, map[string]any{
			"@type": "BreadcrumbList",
			"itemListElement": []any{
				map[string]any{"@type": "ListItem", "position": 1, "name": "خانه", "item": h.absoluteURL("/")},
				map[string]any{"@type": "ListItem", "position": 2, "name": categoryLabel, "item": h.absoluteURL("/products/" + categorySlug)},
			},
		})
	}

	if len(products) > 0 {
		items := make([]any, 0, len(products))
		for i, p := range products {
			availability := "https://schema.org/OutOfStock"
			if p.StockQuantity > 0 {
				availability = "https://schema.org/InStock"
			}
			imgs := p.Images
			if len(imgs) == 0 && p.ImageURL != "" {
				imgs = []string{p.ImageURL}
			}
			absImgs := make([]string, 0, len(imgs))
			for _, im := range imgs {
				absImgs = append(absImgs, h.absoluteURL(im))
			}
			node := map[string]any{
				"@type":       "Product",
				"name":        p.Name,
				"description": p.Description,
				"image":       absImgs,
				"offers": map[string]any{
					"@type":         "Offer",
					"price":         strconv.Itoa(p.Price),
					"priceCurrency": "IRT",
					"availability":  availability,
				},
			}
			items = append(items, map[string]any{
				"@type":    "ListItem",
				"position": i + 1,
				"item":     node,
			})
		}
		graph = append(graph, map[string]any{
			"@type":           "ItemList",
			"itemListElement": items,
		})
	}

	doc := map[string]any{"@context": "https://schema.org", "@graph": graph}
	b, err := json.Marshal(doc)
	if err != nil {
		return ""
	}
	return template.HTML(`<script type="application/ld+json">` + string(b) + `</script>`)
}
