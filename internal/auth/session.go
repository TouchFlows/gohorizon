package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// SessionCookie is the name of the cookie the dashboard carries. It is HttpOnly: the
// bundle never reads it, it only rides along with the fetch calls.
const SessionCookie = "gohorizon_session"

// DefaultTTL is how long a session lives when the configuration does not say. It is short
// because a stateless token cannot be recalled from a client that copied it.
const DefaultTTL = 12 * time.Hour

// MinKeyBytes is the length below which the signing key is refused. A short key makes the
// HMAC forgeable, and a session that can be forged is worse than no session at all.
const MinKeyBytes = 32

// loginWindow and loginMaxFailures bound how fast a password can be guessed: this many
// failures from one address lock that address out for one window.
const (
	loginWindow      = time.Minute
	loginMaxFailures = 5
)

// ErrInvalidCredentials is returned for an unknown user and a wrong password alike, so
// the answer cannot be used to discover which names exist.
var ErrInvalidCredentials = errors.New("invalid credentials")

// RateLimitedError says the caller may try again after RetryAfter.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("too many sign-in attempts; retry in %s", e.RetryAfter)
}

// Identity is a signed-in user and when that session expires.
type Identity struct {
	Name      string    `json:"user"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Store verifies passwords and signs sessions. It holds no server state beyond the
// revocation list, so a restart costs a re-sign-in only for users who signed out and
// whose tokens had not yet expired.
type Store struct {
	users   map[string]string
	dummy   string // a real hash verified against for unknown names, to equalise timing
	key     []byte
	ttl     time.Duration
	now     func() time.Time
	revoked map[string]time.Time // jti -> token expiry
	limiter *limiter
}

// Options configures a Store. Users maps a name to its encoded password hash.
type Options struct {
	Users map[string]string
	// Key signs session tokens. It comes from the environment, never from the file.
	Key []byte
	// TTL is how long a session lives; zero means DefaultTTL.
	TTL time.Duration
	// Now is the clock; tests replace it so expiry and lockout are not slept through.
	Now func() time.Time
}

func New(opts Options) (*Store, error) {
	if len(opts.Users) == 0 {
		return nil, errors.New("auth: at least one user is required")
	}
	if len(opts.Key) < MinKeyBytes {
		return nil, fmt.Errorf("auth: the session key must be at least %d bytes", MinKeyBytes)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	for name, hash := range opts.Users {
		if err := ValidateHash(hash); err != nil {
			return nil, fmt.Errorf("auth: user %q: %w", name, err)
		}
	}
	dummy, err := HashPassword("this password is never correct")
	if err != nil {
		return nil, err
	}
	users := make(map[string]string, len(opts.Users))
	for name, hash := range opts.Users {
		users[name] = hash
	}
	return &Store{
		users:   users,
		dummy:   dummy,
		key:     opts.Key,
		ttl:     ttl,
		now:     now,
		revoked: map[string]time.Time{},
		limiter: newLimiter(now),
	}, nil
}

// Authenticate checks a name and password. The answer is ErrInvalidCredentials for a
// wrong password and for an unknown name, and a *RateLimitedError once that address has
// failed too often inside the window.
func (s *Store) Authenticate(r *http.Request, name, password string) (Identity, error) {
	address := clientAddress(r)
	if wait, ok := s.limiter.blocked(address); !ok {
		return Identity{}, &RateLimitedError{RetryAfter: wait}
	}

	hash, known := s.users[name]
	if !known {
		// Verify against a real hash anyway, so an unknown name costs the same as a wrong
		// password and cannot be timed apart.
		VerifyPassword(s.dummy, password)
		s.limiter.failed(address)
		return Identity{}, ErrInvalidCredentials
	}
	if !VerifyPassword(hash, password) {
		s.limiter.failed(address)
		return Identity{}, ErrInvalidCredentials
	}

	s.limiter.signedIn(address)
	expires := s.now().Add(s.ttl)
	return Identity{Name: name, ExpiresAt: expires.UTC()}, nil
}

// Start writes the session cookie for identity. The token carries the name, the expiry
// and an id, and is signed so the server can trust it without storing it.
func (s *Store) Start(w http.ResponseWriter, r *http.Request, identity Identity) {
	token, err := s.sign(identity)
	if err != nil {
		// A failed sign is a failure to sign in: refuse rather than hand out a session
		// the server would not accept back.
		http.Error(w, "could not start session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.ttl.Seconds()),
	})
}

// End revokes the session the request carries and clears the cookie. Revoking keeps the
// token from working until it expires even though it is a copy of a signed value; the
// revocation list is memory, so it is lost on restart (docs/adr/0009).
func (s *Store) End(w http.ResponseWriter, r *http.Request) {
	if token, ok := s.token(r); ok {
		if claims, err := s.open(token); err == nil {
			s.revoked[claims.ID] = claims.expiry()
		}
	}
	s.prune()
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// Current returns the identity the request carries, if the cookie is present, correctly
// signed, unexpired and not revoked.
func (s *Store) Current(r *http.Request) (Identity, bool) {
	token, ok := s.token(r)
	if !ok {
		return Identity{}, false
	}
	claims, err := s.open(token)
	if err != nil {
		return Identity{}, false
	}
	if _, gone := s.revoked[claims.ID]; gone {
		return Identity{}, false
	}
	if !s.now().Before(claims.expiry()) {
		return Identity{}, false
	}
	return Identity{Name: claims.Subject, ExpiresAt: claims.expiry().UTC()}, true
}

type claims struct {
	Subject string `json:"sub"`
	Expires int64  `json:"exp"`
	ID      string `json:"jti"`
}

func (c claims) expiry() time.Time { return time.Unix(c.Expires, 0).UTC() }

func (s *Store) sign(identity Identity) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims{
		Subject: identity.Name,
		Expires: identity.ExpiresAt.Unix(),
		ID:      hex.EncodeToString(nonce),
	})
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return "v1." + encoded + "." + base64.RawURLEncoding.EncodeToString(s.mac(encoded)), nil
}

func (s *Store) open(token string) (claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return claims{}, errors.New("malformed session token")
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || subtle.ConstantTimeCompare(mac, s.mac(parts[1])) != 1 {
		return claims{}, errors.New("bad session signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims{}, err
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return claims{}, err
	}
	if c.Subject == "" || c.ID == "" {
		return claims{}, errors.New("incomplete session token")
	}
	return c, nil
}

func (s *Store) mac(encodedPayload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("gohorizon-session-v1."))
	m.Write([]byte(encodedPayload))
	return m.Sum(nil)
}

func (s *Store) token(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return cookie.Value, true
}

// revoked returns the ids whose revocation entry can be dropped: the token has expired,
// so the signature already refuses it.
func (s *Store) prune() {
	now := s.now()
	for id, expires := range s.revoked {
		if !now.Before(expires) {
			delete(s.revoked, id)
		}
	}
}

// cookieSecure marks the cookie Secure when the request itself arrived over TLS. A Secure
// cookie is silently dropped over plain HTTP, so the flag follows the transport instead of
// a setting an operator can get wrong in either direction; behind a TLS-terminating proxy
// the browser reaches the proxy over HTTPS and the proxy sets X-Forwarded-Proto, which the
// Secure flag does not depend on because the cookie belongs to the browser's origin.
func cookieSecure(r *http.Request) bool {
	return r.TLS != nil
}

func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter counts failed sign-ins per address in the current window.
type limiter struct {
	now     func() time.Time
	entries map[string]*attempts
}

type attempts struct {
	failures int
	window   time.Time
	blocked  time.Time
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, entries: map[string]*attempts{}}
}

func (l *limiter) blocked(address string) (time.Duration, bool) {
	entry, ok := l.entries[address]
	if !ok {
		return 0, true
	}
	now := l.now()
	if !entry.blocked.IsZero() {
		if now.Before(entry.blocked) {
			return entry.blocked.Sub(now), false
		}
		delete(l.entries, address)
		return 0, true
	}
	if now.Sub(entry.window) >= loginWindow {
		delete(l.entries, address)
	}
	return 0, true
}

func (l *limiter) failed(address string) {
	now := l.now()
	entry, ok := l.entries[address]
	if !ok || now.Sub(entry.window) >= loginWindow {
		entry = &attempts{window: now}
		l.entries[address] = entry
	}
	entry.failures++
	if entry.failures >= loginMaxFailures {
		entry.blocked = now.Add(loginWindow)
	}
}

func (l *limiter) signedIn(address string) {
	delete(l.entries, address)
}
