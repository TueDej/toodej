package handlers

import "net/http"

// BasicAuth returns an HTTP middleware that protects routes with HTTP Basic
// Authentication. Credentials are compared against the provided username and
// password (typically loaded from environment variables ADMIN_USER / ADMIN_PASS).
func BasicAuth(username, password string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || u != username || p != password {
				w.Header().Set("WWW-Authenticate", `Basic realm="Toodej Admin"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
