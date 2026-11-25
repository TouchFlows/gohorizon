package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gohorizon/gohorizon/internal/auth"
	"github.com/gohorizon/gohorizon/internal/health"
)

func authedRouter(t *testing.T) (http.Handler, *auth.Store) {
	t.Helper()
	hash, err := auth.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	store, err := auth.New(auth.Options{
		Users: map[string]string{"alice": hash},
		Key:   []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	provider := &stubProvider{report: health.Report{Status: "ok"}}
	return NewRouter(provider, log, WithAuth(store)), store
}

func serve(handler http.Handler, method, target, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	request.RemoteAddr = "192.0.2.10:5555"
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func sessionCookie(t *testing.T, recorder *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == auth.SessionCookie && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatalf("no %s cookie in %v", auth.SessionCookie, recorder.Result().Cookies())
	return nil
}

func TestOperationalEndpointsRequireASession(t *testing.T) {
	handler, _ := authedRouter(t)

	for _, target := range []string{"/api/v1/health", "/api/v1/metrics?queue=default", "/api/v1/jobs"} {
		recorder := serve(handler, http.MethodGet, target, "")
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", target, recorder.Code)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", target, got)
		}
	}

	// The scrape and the shell stay reachable: Prometheus cannot hold a session, and the
	// bundle is not a secret (its data is).
	if recorder := serve(handler, http.MethodGet, "/metrics", ""); recorder.Code != http.StatusOK {
		t.Errorf("/metrics: status = %d, want 200", recorder.Code)
	}
	if recorder := serve(handler, http.MethodGet, "/", ""); recorder.Code != http.StatusOK {
		t.Errorf("dashboard: status = %d, want 200", recorder.Code)
	}
}

func TestSignInGrantsAccessAndSignOutTakesItBack(t *testing.T) {
	handler, _ := authedRouter(t)

	recorder := serve(handler, http.MethodPost, "/api/v1/session", `{"name":"alice","password":"s3cret"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("sign-in: status = %d, body %s", recorder.Code, recorder.Body)
	}
	var identity auth.Identity
	if err := json.Unmarshal(recorder.Body.Bytes(), &identity); err != nil {
		t.Fatalf("sign-in body: %v", err)
	}
	if identity.Name != "alice" || identity.ExpiresAt.IsZero() {
		t.Fatalf("identity = %+v", identity)
	}
	cookie := sessionCookie(t, recorder)

	if recorder := serve(handler, http.MethodGet, "/api/v1/health", "", cookie); recorder.Code != http.StatusOK {
		t.Fatalf("health after sign-in: status = %d", recorder.Code)
	}
	recorder = serve(handler, http.MethodGet, "/api/v1/session", "", cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("session: status = %d", recorder.Code)
	}

	if recorder := serve(handler, http.MethodDelete, "/api/v1/session", "", cookie); recorder.Code != http.StatusNoContent {
		t.Fatalf("sign-out: status = %d, want 204", recorder.Code)
	}
	if recorder := serve(handler, http.MethodGet, "/api/v1/health", "", cookie); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("health after sign-out: status = %d, want 401", recorder.Code)
	}
}

func TestSignInAnswersWrongPasswordLikeAnUnknownUser(t *testing.T) {
	handler, _ := authedRouter(t)

	wrong := serve(handler, http.MethodPost, "/api/v1/session", `{"name":"alice","password":"nope"}`)
	unknown := serve(handler, http.MethodPost, "/api/v1/session", `{"name":"mallory","password":"s3cret"}`)
	malformed := serve(handler, http.MethodPost, "/api/v1/session", `not json`)

	if wrong.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized {
		t.Fatalf("statuses = %d and %d, want 401", wrong.Code, unknown.Code)
	}
	if wrong.Body.String() != unknown.Body.String() {
		t.Errorf("wrong password says %q, unknown user says %q; the answers must match", wrong.Body, unknown.Body)
	}
	if malformed.Code != http.StatusBadRequest {
		t.Errorf("malformed body: status = %d, want 400", malformed.Code)
	}
}

func TestSignInIsRateLimited(t *testing.T) {
	handler, _ := authedRouter(t)

	for i := 0; i < 5; i++ {
		if recorder := serve(handler, http.MethodPost, "/api/v1/session", `{"name":"alice","password":"nope"}`); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, recorder.Code)
		}
	}
	// The fifth failure locks the address; the next attempt is refused without checking
	// the password at all.
	limited := serve(handler, http.MethodPost, "/api/v1/session", `{"name":"alice","password":"nope"}`)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("after five failures: status = %d, want 429", limited.Code)
	}
	if limited.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	// The correct password is refused too while the address is locked out; otherwise the
	// limit would only slow down guesses of wrong passwords.
	if recorder := serve(handler, http.MethodPost, "/api/v1/session", `{"name":"alice","password":"s3cret"}`); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("locked-out address with the right password: status = %d, want 429", recorder.Code)
	}
}

func TestWithoutUsersTheApiStaysOpen(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	provider := &stubProvider{report: health.Report{Status: "ok"}}

	recorder := serve(NewRouter(provider, log), http.MethodGet, "/api/v1/health", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 without auth configured", recorder.Code)
	}
	// There is no session route to call when no users exist.
	if recorder := serve(NewRouter(provider, log), http.MethodGet, "/api/v1/session", ""); recorder.Code == http.StatusOK {
		t.Fatal("a session was answered with no users configured")
	}
}
