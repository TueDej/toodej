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

	urls := []sitemapURL{
		{Loc: baseURL + "/", LastMod: today, ChangeFreq: "daily", Priority: "1.0"},
		{Loc: baseURL + "/products", LastMod: today, ChangeFreq: "weekly", Priority: "0.8"},
		{Loc: baseURL + "/our-story", LastMod: today, ChangeFreq: "monthly", Priority: "0.5"},
	}

	products, err := database.GetProducts(h.db, "")
	if err != nil {
		logutil.Error("sitemap: get products", "err", err)
	} else {
		for _, p := range products {
			lastMod := p.CreatedAt.UTC().Format("2006-01-02")
			urls = append(urls, sitemapURL{
				Loc:        baseURL + "/product/" + p.Slug,
				LastMod:    lastMod,
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
