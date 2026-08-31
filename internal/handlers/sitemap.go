package handlers

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"time"

	"farmstore/internal/database"
	"farmstore/internal/logutil"
)

const baseURL = "https://toodej.shop"

type sitemapURL struct {
	Loc        string `xml:"loc"`
	LastMod    string `xml:"lastmod,omitempty"`
	ChangeFreq string `xml:"changefreq"`
	Priority   string `xml:"priority"`
}

type sitemapRoot struct {
	XMLName xml.Name     `xml:"urlset"`
	Xmlns   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

func (h *Handler) ServeSitemap(w http.ResponseWriter, r *http.Request) {
	today := time.Now().UTC().Format("2006-01-02")

	// Only list URLs that actually serve a 200. The storefront has no
	// per-product detail route (products are shown in category grids), so the
	// sitemap points at the home page, the all-products listing, the about
	// page, and one entry per enabled category — matching the real routes in
	// cmd/server/main.go. Emitting /product/{slug} here caused 404s that
	// blocked Google indexing.
	urls := []sitemapURL{
		{Loc: baseURL + "/", LastMod: today, ChangeFreq: "daily", Priority: "1.0"},
		{Loc: baseURL + "/products/all", LastMod: today, ChangeFreq: "weekly", Priority: "0.8"},
		{Loc: baseURL + "/about", LastMod: today, ChangeFreq: "monthly", Priority: "0.5"},
	}

	cats, err := database.GetEnabledCategories(r.Context(), h.db)
	if err != nil {
		logutil.Error("sitemap: get categories", "err", err)
	} else {
		for _, c := range cats {
			urls = append(urls, sitemapURL{
				Loc:        baseURL + "/products/" + c.Slug,
				LastMod:    today,
				ChangeFreq: "weekly",
				Priority:   "0.7",
			})
		}
	}

	root := sitemapRoot{Xmlns: "http://www.sitemaps.org/schemas/sitemap/0.9", URLs: urls}

	output, err := xml.MarshalIndent(root, "", "  ")
	if err != nil {
		logutil.Error("sitemap: marshal", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	fmt.Fprint(w, xml.Header)
	w.Write(output)
}
