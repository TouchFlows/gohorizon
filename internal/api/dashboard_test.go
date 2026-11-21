package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gohorizon/gohorizon/web"
)

// builtAsset names one hashed file of the committed bundle, so these tests serve what Vite
// emitted rather than a name written into a test.
func builtAsset(t *testing.T) string {
	t.Helper()

	dir, err := web.Dist().Open("/assets")
	if err != nil {
		t.Fatalf("open the built assets: %v", err)
	}
	defer dir.Close()
	entries, err := dir.Readdir(-1)
	if err != nil {
		t.Fatalf("list the built assets: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".js") {
			return "/assets/" + entry.Name()
		}
	}
	t.Fatal("the bundle has no JavaScript asset; run npm run build in web/ and commit dist")
	return ""
}

func TestDashboardServesTheShell(t *testing.T) {
	recorder := get(t, &stubProvider{}, "/")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("content type = %q, want HTML", got)
	}
	// index.html names the current build's assets, so it must not outlive the deploy.
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache control = %q, want no-store", got)
	}
	if !strings.Contains(recorder.Body.String(), `<div id="root"></div>`) {
		t.Errorf("body = %s, want the shell", recorder.Body.String())
	}
}

func TestDashboardServesHashedAssetsAsImmutable(t *testing.T) {
	asset := builtAsset(t)
	recorder := get(t, &stubProvider{}, asset)

	if recorder.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200", asset, recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Errorf("%s: content type = %q, want JavaScript", asset, got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != immutableAssets {
		t.Errorf("%s: cache control = %q, want the immutable policy", asset, got)
	}
	if recorder.Body.Len() == 0 {
		t.Errorf("%s: empty body", asset)
	}
}

func TestDashboardServesClientRoutesWithTheShell(t *testing.T) {
	for _, target := range []string{"/jobs/abc", "/queues/default/workers", "/settings"} {
		recorder := get(t, &stubProvider{}, target)

		if recorder.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 from the shell", target, recorder.Code)
			continue
		}
		if !strings.Contains(recorder.Body.String(), `<div id="root"></div>`) {
			t.Errorf("%s: body is not the shell", target)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: cache control = %q, want no-store", target, got)
		}
	}
}

func TestDashboardDoesNotInventAssets(t *testing.T) {
	// A missing hashed asset is a 404, not the shell: a browser would otherwise run index.html
	// as if it were the script it asked for.
	if code := get(t, &stubProvider{}, "/assets/does-not-exist.js").Code; code != http.StatusNotFound {
		t.Errorf("missing asset: status = %d, want 404", code)
	}
}

func TestDashboardLeavesTheAPIToItsRoutes(t *testing.T) {
	healthCheck := get(t, &stubProvider{}, "/api/v1/health")
	if got := healthCheck.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("/api/v1/health content type = %q, want JSON", got)
	}

	unknown := get(t, &stubProvider{}, "/api/v1/nope")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown API path: status = %d, want 404", unknown.Code)
	}
	if got := unknown.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("unknown API path content type = %q, want JSON", got)
	}
	if strings.Contains(unknown.Body.String(), `<div id="root">`) {
		t.Errorf("unknown API path returned the shell: %s", unknown.Body.String())
	}
}

func TestDashboardRejectsOtherMethods(t *testing.T) {
	recorder := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	NewRouter(&stubProvider{}, log).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/jobs", nil))

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
	if got := recorder.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("allow = %q, want the methods the shell answers", got)
	}
}
