package metrics

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// Path is where the metrics are answered, the path every scraper asks by default.
const Path = "/metrics"

// Handler answers a scrape of r at Path, to a request bearing the token whose SHA-256 is hash, and
// 401 to any other.
//
// A token, although the listener is meant for a network only the monitoring reaches, because the
// metrics name every namespace and workflow that ran and every runner of the installation: a tenant
// who reached the port would read the other tenants' activity. The token is compared as its hash,
// so the file the program reads it from is worth nothing to somebody who reads it, and in constant
// time besides.
//
// The answer is written whole before any of it is sent, so that a scrape cut short by a gauge that
// took too long is a failed scrape rather than half an answer the scraper would store.
func Handler(r *Registry, hash [sha256.Size]byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != Path {
			http.NotFound(w, req)
			return
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "the metrics are read with GET", http.StatusMethodNotAllowed)
			return
		}
		token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		presented := sha256.Sum256([]byte(token))
		if !ok || token == "" || subtle.ConstantTimeCompare(presented[:], hash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="agentiik metrics"`)
			http.Error(w, "the metrics are read with the token whose hash AGK_METRICS_TOKEN_FILE holds", http.StatusUnauthorized)
			return
		}
		var body bytes.Buffer
		if err := r.WriteTo(req.Context(), &body); err != nil {
			http.Error(w, "the metrics could not be written", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", ContentType)
		w.Header().Set("Cache-Control", "no-store")
		if req.Method == http.MethodHead {
			return
		}
		w.Write(body.Bytes())
	})
}
