package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gohorizon/gohorizon/web"
)

// immutableAssets is how long a hashed asset may be cached. Vite names each file after its
// contents, so a new build produces a new name and a name that exists never changes.
const immutableAssets = "public, max-age=31536000, immutable"

// dashboard serves the embedded shell (docs/adr/0008). Everything the versioned API does
// not claim lands here: a file of the bundle if there is one, index.html otherwise, so a
// client route deep-links instead of 404ing.
func dashboard() http.HandlerFunc {
	assets := web.Dist()
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		if file, info, ok := openBundleFile(assets, path.Clean(r.URL.Path)); ok {
			defer file.Close()
			// Only the hashed bundle is immutable. index.html names the current build, so
			// caching it would serve a page pointing at assets the next deploy removed.
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				w.Header().Set("Cache-Control", immutableAssets)
			} else {
				w.Header().Set("Cache-Control", "no-store")
			}
			http.ServeContent(w, r, info.Name(), info.ModTime(), file)
			return
		}

		// A missing hashed asset is a 404, never the shell: the browser asked for a script,
		// and index.html is not one. Only paths outside the bundle are client routes.
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}

		serveDashboardIndex(w, r, assets)
	}
}

// openBundleFile opens one file of the bundle. A directory is absent: "/" is the shell, not
// a listing.
func openBundleFile(assets http.FileSystem, name string) (http.File, fs.FileInfo, bool) {
	file, err := assets.Open(name)
	if err != nil {
		return nil, nil, false
	}
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		file.Close()
		return nil, nil, false
	}
	return file, info, true
}

// serveDashboardIndex answers with the shell, which is what makes a deep link work: the
// route exists only in the browser.
func serveDashboardIndex(w http.ResponseWriter, r *http.Request, assets http.FileSystem) {
	file, info, ok := openBundleFile(assets, "/index.html")
	if !ok {
		writeError(w, http.StatusNotFound, "this binary was built without the dashboard")
		return
	}
	defer file.Close()
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}
