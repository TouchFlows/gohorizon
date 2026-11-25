package auth

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func mustHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return hash
}

func TestPasswordHashRoundTrip(t *testing.T) {
	hash := mustHash(t, "correct horse battery staple")

	if !strings.HasPrefix(hash, "pbkdf2-sha256$210000$") {
		t.Fatalf("hash %q does not carry its scheme and iteration count", hash)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("the right password did not verify")
	}
	if VerifyPassword(hash, "correct horse battery stapler") {
		t.Fatal("a wrong password verified")
	}
	if err := ValidateHash(hash); err != nil {
		t.Fatalf("ValidateHash rejected a hash it produced: %v", err)
	}

	// Two hashes of the same password differ: the salt is per password, so a leaked hash
	// cannot be compared against another account's.
	if other := mustHash(t, "correct horse battery staple"); other == hash {
		t.Fatal("two hashes of one password are identical; the salt is not random")
	}
}

func TestHashPasswordRefusesAnEmptyPassword(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Fatal("an empty password was hashed")
	}
}

func TestValidateHashRejectsMalformedHashes(t *testing.T) {
	good := mustHash(t, "x")
	parts := strings.Split(good, "$")
	cases := map[string]string{
		"empty":       "",
		"no parts":    "not-a-hash",
		"scheme":      "bcrypt$210000$" + parts[2] + "$" + parts[3],
		"iterations":  "pbkdf2-sha256$many$" + parts[2] + "$" + parts[3],
		"zero iters":  "pbkdf2-sha256$0$" + parts[2] + "$" + parts[3],
		"salt":        "pbkdf2-sha256$210000$c2hvcnQ$" + parts[3],
		"salt base64": "pbkdf2-sha256$210000$***$" + parts[3],
		"key":         "pbkdf2-sha256$210000$" + parts[2] + "$c2hvcnQ",
	}
	for name, encoded := range cases {
		if err := ValidateHash(encoded); err == nil {
			t.Errorf("%s: ValidateHash accepted %q", name, encoded)
		}
		if VerifyPassword(encoded, "x") {
			t.Errorf("%s: VerifyPassword accepted %q", name, encoded)
		}
	}
}

func newTestStore(t *testing.T, now func() time.Time) *Store {
	t.Helper()
	store, err := New(Options{
		Users: map[string]string{"alice": mustHash(t, "s3cret")},
		Key:   []byte("0123456789abcdef0123456789abcdef"),
		Now:   now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

func signInRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/session", nil)
	r.RemoteAddr = "192.0.2.10:5555"
	return r
}

func TestAuthenticateStartsASessionTheRequestCarriesBack(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	store := newTestStore(t, func() time.Time { return now })

	identity, err := store.Authenticate(signInRequest(), "alice", "s3cret")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.Name != "alice" || !identity.ExpiresAt.Equal(now.Add(DefaultTTL)) {
		t.Fatalf("identity = %+v, want alice expiring at %v", identity, now.Add(DefaultTTL))
	}

	recorder := httptest.NewRecorder()
	store.Start(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/session", nil), identity)
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != SessionCookie {
		t.Fatalf("cookies = %v, want one %s", cookies, SessionCookie)
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie %+v is not HttpOnly/Lax", cookies[0])
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	r.AddCookie(cookies[0])
	current, ok := store.Current(r)
	if !ok {
		t.Fatal("a session the store started was not accepted back")
	}
	if current.Name != "alice" {
		t.Fatalf("current = %+v, want alice", current)
	}
}

func TestCookieIsSecureWhateverTerminatedTLS(t *testing.T) {
	// The proxy case is the one that matters: the browser speaks HTTPS to a proxy that
	// forwards plain HTTP, so the request carries no TLS state and only the header says the
	// cookie will travel over TLS. The header is believed only from a peer in the trusted
	// list, so the same case from a client that is not a proxy is read differently.
	cases := []struct {
		name    string
		tls     bool
		proto   string
		proxied bool // the request arrives from a peer listed as a trusted proxy
		want    bool
	}{
		{name: "plain http", want: false},
		{name: "direct tls", tls: true, want: true},
		{name: "trusted proxy terminated tls", proto: "https", proxied: true, want: true},
		{name: "chain of proxies", proto: "https, http", proxied: true, want: true},
		{name: "mixed case and spaces", proto: " HTTPS ", proxied: true, want: true},
		{name: "trusted proxy forwarded plain http", proto: "http", proxied: true, want: false},
		// A header is worth what its sender is: a client that is not a proxy cannot have its
		// cookie marked Secure, and so cannot make the browser drop it either.
		{name: "untrusted peer claiming tls", proto: "https", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t, nil)
			remote := "192.0.2.5:5555"
			if tc.proxied {
				store = proxyStore(t, nil)
				remote = "10.0.0.2:5555"
			}

			signIn := httptest.NewRequest(http.MethodPost, "/api/v1/session", nil)
			signIn.RemoteAddr = remote
			identity, err := store.Authenticate(signIn, "alice", "s3cret")
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}

			r := httptest.NewRequest(http.MethodPost, "/api/v1/session", nil)
			r.RemoteAddr = remote
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if tc.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			recorder := httptest.NewRecorder()
			if err := store.Start(recorder, r, identity); err != nil {
				t.Fatalf("Start: %v", err)
			}
			cookies := recorder.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("cookies = %v, want one", cookies)
			}
			if cookies[0].Secure != tc.want {
				t.Errorf("Secure = %v, want %v", cookies[0].Secure, tc.want)
			}
		})
	}
}

func TestRemovingAUserEndsTheirSession(t *testing.T) {
	store := newTestStore(t, nil)
	identity, err := store.Authenticate(signInRequest(), "alice", "s3cret")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	recorder := httptest.NewRecorder()
	if err := store.Start(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/session", nil), identity); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	r.AddCookie(recorder.Result().Cookies()[0])
	if _, ok := store.Current(r); !ok {
		t.Fatal("the session was not accepted before the user was removed")
	}

	// The same signing key and the same token, but a configuration that no longer has alice:
	// removing an operator has to end their access on the next request rather than when the
	// signature happens to expire.
	remaining, err := New(Options{
		Users: map[string]string{"bob": mustHash(t, "s3cret")},
		Key:   []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := remaining.Current(r); ok {
		t.Fatal("a token naming a user the configuration dropped still authenticated")
	}
}

func TestAuthenticateRefusesWrongPasswordAndUnknownUserAlike(t *testing.T) {
	store := newTestStore(t, nil)

	if _, err := store.Authenticate(signInRequest(), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v, want ErrInvalidCredentials", err)
	}
	if _, err := store.Authenticate(signInRequest(), "mallory", "s3cret"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user: %v, want ErrInvalidCredentials", err)
	}
}

func TestSessionExpires(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	store := newTestStore(t, func() time.Time { return now })

	identity, err := store.Authenticate(signInRequest(), "alice", "s3cret")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	recorder := httptest.NewRecorder()
	store.Start(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/session", nil), identity)
	cookie := recorder.Result().Cookies()[0]

	r := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	r.AddCookie(cookie)

	now = now.Add(DefaultTTL - time.Second)
	if _, ok := store.Current(r); !ok {
		t.Fatal("the session expired before its time")
	}
	now = now.Add(time.Second)
	if _, ok := store.Current(r); ok {
		t.Fatal("the session outlived its expiry")
	}
}

func TestTamperedTokenIsRefused(t *testing.T) {
	store := newTestStore(t, nil)
	identity, err := store.Authenticate(signInRequest(), "alice", "s3cret")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	recorder := httptest.NewRecorder()
	store.Start(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/session", nil), identity)
	value := recorder.Result().Cookies()[0].Value

	// Flip a character of the payload; the signature cannot match it any more.
	tampered := []byte(value)
	tampered[len("v1.")] ^= 0x01
	r := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: string(tampered)})
	if _, ok := store.Current(r); ok {
		t.Fatal("a token with an edited payload was accepted")
	}

	// A token signed with a different key is refused too, which is what makes rotating
	// the key sign everyone out.
	stranger, err := New(Options{
		Users: map[string]string{"alice": mustHash(t, "s3cret")},
		Key:   []byte("ffffffffffffffffffffffffffffffff"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r = httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: value})
	if _, ok := stranger.Current(r); ok {
		t.Fatal("a token signed with one key was accepted by another")
	}
}

func TestSignOutRevokesTheTokenItWasGiven(t *testing.T) {
	store := newTestStore(t, nil)
	identity, err := store.Authenticate(signInRequest(), "alice", "s3cret")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	recorder := httptest.NewRecorder()
	store.Start(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/session", nil), identity)
	cookie := recorder.Result().Cookies()[0]

	r := httptest.NewRequest(http.MethodDelete, "/api/v1/session", nil)
	r.AddCookie(cookie)
	store.End(httptest.NewRecorder(), r)

	// The browser throws its cookie away, but a copy of the token must stop working too.
	if _, ok := store.Current(r); ok {
		t.Fatal("a signed-out token still authenticates")
	}

	// A restart loses the revocation list, as the ADR says; the cookie is gone from the
	// browser, and the token expires within the TTL.
	restarted := newTestStore(t, nil)
	if _, ok := restarted.Current(r); !ok {
		t.Fatal("the test expected a restart to lose the revocation list")
	}
}

func TestSignInIsRateLimitedPerAddress(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	store := newTestStore(t, func() time.Time { return now })

	for i := 0; i < loginMaxFailures; i++ {
		if _, err := store.Authenticate(signInRequest(), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v, want ErrInvalidCredentials", i+1, err)
		}
	}

	_, err := store.Authenticate(signInRequest(), "alice", "s3cret")
	var limited *RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("after %d failures: %v, want a RateLimitedError", loginMaxFailures, err)
	}
	if limited.RetryAfter <= 0 || limited.RetryAfter > loginWindow {
		t.Fatalf("RetryAfter = %v, want within one window", limited.RetryAfter)
	}

	// Another address is unaffected: the limit is on the guesser, not on the account.
	other := signInRequest()
	other.RemoteAddr = "192.0.2.11:5555"
	if _, err := store.Authenticate(other, "alice", "s3cret"); err != nil {
		t.Fatalf("a different address was blocked: %v", err)
	}

	now = now.Add(loginWindow)
	if _, err := store.Authenticate(signInRequest(), "alice", "s3cret"); err != nil {
		t.Fatalf("the lockout outlived its window: %v", err)
	}
}

// proxyStore trusts one proxy range, which is the deployment the limiter used to get wrong:
// every request arrives from the proxy, so the peer is never the client.
func proxyStore(t *testing.T, now func() time.Time) *Store {
	t.Helper()
	store, err := New(Options{
		Users:          map[string]string{"alice": mustHash(t, "s3cret")},
		Key:            []byte("0123456789abcdef0123456789abcdef"),
		Now:            now,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

func TestClientAddressIsWhereTheGuessCameFrom(t *testing.T) {
	store := proxyStore(t, nil)

	cases := []struct {
		name      string
		remote    string
		forwarded string
		also      string // a second X-Forwarded-For line, which proxies append
		want      string
	}{
		{name: "direct client", remote: "192.0.2.5:1234", want: "192.0.2.5"},
		{name: "trusted proxy", remote: "10.0.0.2:1234", forwarded: "203.0.113.7", want: "203.0.113.7"},
		{name: "proxy chain", remote: "10.0.0.2:1234", forwarded: "203.0.113.7, 10.0.0.5", want: "203.0.113.7"},
		{name: "appended header line", remote: "10.0.0.2:1234", forwarded: "203.0.113.7", also: "10.0.0.5", want: "203.0.113.7"},
		{name: "every hop trusted", remote: "10.0.0.2:1234", forwarded: "10.0.0.9, 10.0.0.5", want: "10.0.0.9"},
		{name: "ipv4-mapped entry", remote: "10.0.0.2:1234", forwarded: "::ffff:203.0.113.7", want: "203.0.113.7"},
		{name: "unreadable chain", remote: "10.0.0.2:1234", forwarded: "203.0.113.7, not-an-address", want: "10.0.0.2"},
		{name: "forged by a direct client", remote: "192.0.2.5:1234", forwarded: "203.0.113.7", want: "192.0.2.5"},
		{name: "trusted proxy with no chain", remote: "10.0.0.2:1234", want: "10.0.0.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/session", nil)
			r.RemoteAddr = tc.remote
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			if tc.also != "" {
				r.Header.Add("X-Forwarded-For", tc.also)
			}
			if got := store.clientAddress(r); got != tc.want {
				t.Errorf("clientAddress = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSignInLimitFollowsTheForwardedClientBehindATrustedProxy(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	store := proxyStore(t, func() time.Time { return now })

	behind := func(forwarded string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/session", nil)
		r.RemoteAddr = "10.0.0.2:4444"
		r.Header.Set("X-Forwarded-For", forwarded)
		return r
	}

	for i := 0; i < loginMaxFailures; i++ {
		if _, err := store.Authenticate(behind("203.0.113.7"), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v, want ErrInvalidCredentials", i+1, err)
		}
	}
	var limited *RateLimitedError
	if _, err := store.Authenticate(behind("203.0.113.7"), "alice", "s3cret"); !errors.As(err, &limited) {
		t.Fatalf("the guessing client was not locked out: %v", err)
	}

	// The point of the change: the next person behind the same proxy still gets in. Keying on
	// the proxy would have counted everyone together and locked them all out.
	if _, err := store.Authenticate(behind("198.51.100.9"), "alice", "s3cret"); err != nil {
		t.Fatalf("a second operator behind the same proxy was locked out: %v", err)
	}
}

func TestForwardedForFromAnUntrustedPeerDoesNotResetTheLimit(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	store := proxyStore(t, func() time.Time { return now })

	direct := func(forwarded string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/session", nil)
		r.RemoteAddr = "192.0.2.5:4444"
		r.Header.Set("X-Forwarded-For", forwarded)
		return r
	}

	// A client that is not a trusted proxy cannot invent a new address per attempt: the count
	// stays on the peer the service can see.
	for i := 0; i < loginMaxFailures; i++ {
		if _, err := store.Authenticate(direct(fmt.Sprintf("203.0.113.%d", i)), "alice", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v, want ErrInvalidCredentials", i+1, err)
		}
	}
	var limited *RateLimitedError
	if _, err := store.Authenticate(direct("198.51.100.9"), "alice", "s3cret"); !errors.As(err, &limited) {
		t.Fatalf("rotating X-Forwarded-For escaped the limit: %v", err)
	}
}

func TestNewRefusesAKeyTooShortToSignWith(t *testing.T) {
	_, err := New(Options{
		Users: map[string]string{"alice": mustHash(t, "s3cret")},
		Key:   []byte("short"),
	})
	if err == nil {
		t.Fatal("a five-byte signing key was accepted")
	}
}

func TestNewRefusesAMalformedUserHash(t *testing.T) {
	_, err := New(Options{
		Users: map[string]string{"alice": "not-a-hash"},
		Key:   []byte("0123456789abcdef0123456789abcdef"),
	})
	if err == nil {
		t.Fatal("a user whose hash cannot verify was accepted")
	}
}
