package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gohorizon/gohorizon/internal/adapter"
	"github.com/gohorizon/gohorizon/internal/api"
	"github.com/gohorizon/gohorizon/internal/auth"
	"github.com/gohorizon/gohorizon/internal/health"
	"github.com/gohorizon/gohorizon/internal/metrics"
)

// idleProvider answers like a service with nothing to report: the session routes and the two
// proxy decisions under test never read it.
type idleProvider struct{}

func (idleProvider) Report() health.Report { return health.Report{Status: "ok"} }

func (idleProvider) Buckets(context.Context, metrics.Resolution, string, time.Time, time.Time) ([]metrics.Bucket, error) {
	return nil, nil
}

func (idleProvider) Jobs(context.Context, metrics.JobQuery) ([]metrics.Job, error) {
	return nil, nil
}

func (idleProvider) Job(context.Context, string) (metrics.Job, error) {
	return metrics.Job{}, nil
}

func (idleProvider) RestartWorker(string) error {
	return errors.New("this test computes nothing")
}

func (idleProvider) RetryFailedJob(context.Context, string) error {
	return errors.New("this test computes nothing")
}

func (idleProvider) ForgetFailedJob(context.Context, string) error {
	return errors.New("this test computes nothing")
}

func (idleProvider) FailedJobs(context.Context, adapter.FailedJobQuery) (adapter.FailedJobsPage, error) {
	return adapter.FailedJobsPage{}, nil
}

// TestTrustedProxyCarriesTheClientAndTheTLS is the proxy path with nothing replaced: the
// configuration an operator writes, the store newAuthStore builds from it, the router the
// service serves, and real HTTP requests whose peer is the proxy it trusts. The test client
// connects from 127.0.0.1, which is the peer named below, so both forwarded headers reach the
// service the way a proxy would send them.
func TestTrustedProxyCarriesTheClientAndTheTLS(t *testing.T) {
	t.Setenv("GOHORIZON_TEST_SESSION_KEY", "0123456789abcdef0123456789abcdef")
	cfg := authConfig(t, "GOHORIZON_TEST_SESSION_KEY")
	cfg.Server.TrustedProxies = []string{"127.0.0.1"}

	store, err := newAuthStore(cfg)
	if err != nil {
		t.Fatalf("newAuthStore: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(api.NewRouter(idleProvider{}, log, api.WithAuth(store)))
	defer server.Close()

	type answer struct {
		status  int
		cookies []*http.Cookie
	}
	post := func(password, forwardedFor, forwardedProto string) answer {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/session",
			strings.NewReader(`{"name":"alice","password":"`+password+`"}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		if forwardedFor != "" {
			request.Header.Set("X-Forwarded-For", forwardedFor)
		}
		if forwardedProto != "" {
			request.Header.Set("X-Forwarded-Proto", forwardedProto)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("post session: %v", err)
		}
		defer response.Body.Close()
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatalf("read body: %v", err)
		}
		return answer{status: response.StatusCode, cookies: response.Cookies()}
	}

	// The proxy reported TLS, so the cookie may only travel over it: the peer's header is the
	// only evidence there was any TLS at all, because the hop to this service is plain HTTP.
	signedIn := post("s3cret", "203.0.113.7", "https")
	if signedIn.status != http.StatusOK {
		t.Fatalf("sign-in behind the proxy: status = %d, want 200", signedIn.status)
	}
	if cookie := sessionCookieFrom(t, signedIn.cookies); !cookie.Secure {
		t.Error("the session cookie is not Secure although the trusted proxy reported https")
	}

	// The same proxy over plain HTTP reports no TLS, and the cookie says so: the header
	// decides, not the mere fact that the request came through a proxy.
	plain := post("s3cret", "203.0.113.7", "http")
	if plain.status != http.StatusOK {
		t.Fatalf("sign-in over plain http: status = %d, want 200", plain.status)
	}
	if cookie := sessionCookieFrom(t, plain.cookies); cookie.Secure {
		t.Error("the session cookie is Secure although the trusted proxy reported http")
	}

	// Five bad guesses from one client the proxy forwarded for...
	for attempt := 1; attempt <= 5; attempt++ {
		if got := post("nope", "203.0.113.7", "https"); got.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", attempt, got.status)
		}
	}
	// ...lock that client out, and only that client: counting the proxy, which is what the
	// limiter saw before the trusted list existed, would have locked out everyone behind it.
	if got := post("s3cret", "203.0.113.7", "https"); got.status != http.StatusTooManyRequests {
		t.Errorf("the guessing client: status = %d, want 429", got.status)
	}
	second := post("s3cret", "198.51.100.9", "https")
	if second.status != http.StatusOK {
		t.Fatalf("a second client behind the same proxy: status = %d, want 200", second.status)
	}

	// The session that client was given is one the API honours, so the cookie is a working
	// credential for the client the proxy named rather than a decorative header.
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/health", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("X-Forwarded-For", "198.51.100.9")
	request.AddCookie(sessionCookieFrom(t, second.cookies))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("health with the session the proxy's client was given: status = %d, want 200", response.StatusCode)
	}
}

// sessionCookieFrom picks the session out of a response's cookies.
func sessionCookieFrom(t *testing.T, cookies []*http.Cookie) *http.Cookie {
	t.Helper()
	for _, cookie := range cookies {
		if cookie.Name == auth.SessionCookie {
			return cookie
		}
	}
	t.Fatalf("no %s cookie in %v", auth.SessionCookie, cookies)
	return nil
}
