package handlers

import (
	"fmt"
	"net/http"
)

func ServeRobotsTXT(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, "User-agent: *\nAllow: /\nDisallow: /admin/\nDisallow: /checkout/\nDisallow: /api/\n\nSitemap: https://toodej.shop/sitemap.xml\n")
}
