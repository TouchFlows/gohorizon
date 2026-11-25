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
	"net/netip"
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

// Roles an operator can hold (handoff §13). A viewer reads, an operator acts on the work the
// pool is running (restarting a worker), and an administrator will own what changes the
// deployment rather than its work. The role is configuration, not a claim: it is read from the
// configured users on every request, so promoting or demoting someone takes effect at their
// next request rather than when their session happens to expire.
const (
	RoleViewer        = "viewer"
	RoleOperator      = "operator"
	RoleAdministrator = "administrator"
)

// Rank orders the roles from least to most privileged. A check names the least role that may
// pass, and every role above it passes too, so a new role has to be placed here rather than
// slipping past an equality test. A role this service does not know ranks below every named
// threshold, so an unrecognised value fails closed.
func Rank(role string) int {
	switch role {
	case RoleOperator:
		return 1
	case RoleAdministrator:
		return 2
	}
	return 0
}

// ValidRole reports whether role is one this service knows.
func ValidRole(role string) bool {
	switch role {
	case RoleViewer, RoleOperator, RoleAdministrator:
		return true
	}
	return false
}

// User is one operator: the hash their password is verified against, and the role a session
// started for them carries.
type User struct {
	PasswordHash string
	// Role is one of the Role constants; empty means RoleViewer.
	Role string
}

// Identity is a signed-in user, their role, and when that session expires.
type Identity struct {
	Name      string    `json:"user"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Store verifies passwords and signs sessions. It holds no server state beyond the
// revocation list, so a restart costs a re-sign-in only for users who signed out and
// whose tokens had not yet expired.
type Store struct {
	users   map[string]User
	dummy   string // a real hash verified against for unknown names, to equalise timing
	key     []byte
	ttl     time.Duration
	now     func() time.Time
	revoked map[string]time.Time // jti -> token expiry
	limiter *limiter
	trusted []netip.Prefix // peers whose X-Forwarded-For is believed
}

// Options configures a Store. Users maps a name to its encoded password hash and role.
type Options struct {
	Users map[string]User
	// Key signs session tokens. It comes from the environment, never from the file.
	Key []byte
	// TTL is how long a session lives; zero means DefaultTTL.
	TTL time.Duration
	// Now is the clock; tests replace it so expiry and lockout are not slept through.
	Now func() time.Time
	// TrustedProxies are the peers whose X-Forwarded-For is believed. A request that did not
	// arrive from one of them is keyed on its own address, so a client cannot rotate the
	// header to outrun the limiter.
	TrustedProxies []netip.Prefix
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
	for name, user := range opts.Users {
		if err := ValidateHash(user.PasswordHash); err != nil {
			return nil, fmt.Errorf("auth: user %q: %w", name, err)
		}
		if user.Role != "" && !ValidRole(user.Role) {
			return nil, fmt.Errorf("auth: user %q: %q is not a role (supported: %s, %s, %s)",
				name, user.Role, RoleViewer, RoleOperator, RoleAdministrator)
		}
	}
	dummy, err := HashPassword("this password is never correct")
	if err != nil {
		return nil, err
	}
	users := make(map[string]User, len(opts.Users))
	for name, user := range opts.Users {
		if user.Role == "" {
			// Least privilege for a user whose role nobody configured.
			user.Role = RoleViewer
		}
		users[name] = user
	}
	return &Store{
		users:   users,
		dummy:   dummy,
		key:     opts.Key,
		ttl:     ttl,
		now:     now,
		revoked: map[string]time.Time{},
		limiter: newLimiter(now),
		trusted: append([]netip.Prefix(nil), opts.TrustedProxies...),
	}, nil
}

// Authenticate checks a name and password. The answer is ErrInvalidCredentials for a
// wrong password and for an unknown name, and a *RateLimitedError once that address has
// failed too often inside the window.
func (s *Store) Authenticate(r *http.Request, name, password string) (Identity, error) {
	address := s.ClientAddress(r)
	if wait, ok := s.limiter.blocked(address); !ok {
		return Identity{}, &RateLimitedError{RetryAfter: wait}
	}

	user, known := s.users[name]
	if !known {
		// Verify against a real hash anyway, so an unknown name costs the same as a wrong
		// password and cannot be timed apart.
		VerifyPassword(s.dummy, password)
		s.limiter.failed(address)
		return Identity{}, ErrInvalidCredentials
	}
	if !VerifyPassword(user.PasswordHash, password) {
		s.limiter.failed(address)
		return Identity{}, ErrInvalidCredentials
	}

	s.limiter.signedIn(address)
	expires := s.now().Add(s.ttl)
	return Identity{Name: name, Role: user.Role, ExpiresAt: expires.UTC()}, nil
}

// Start writes the session cookie for identity. The token carries the name, the expiry
// and an id, and is signed so the server can trust it without storing it. A failure to sign
// is returned rather than written: the handler owns the one answer to the request, and a
// store that answered here too would write a second status line into the same response.
func (s *Store) Start(w http.ResponseWriter, r *http.Request, identity Identity) error {
	token, err := s.sign(identity)
	if err != nil {
		// A failed sign is a failure to sign in: refuse rather than hand out a session
		// the server would not accept back.
		return fmt.Errorf("sign session: %w", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.ttl.Seconds()),
	})
	return nil
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
		Secure:   s.cookieSecure(r),
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
	// A token outlives the account it names: the configuration can drop a user, or change
	// their role, while their session is still signed. Both are read from the configured
	// users here, so removing someone ends their access at the next request and a demotion
	// takes effect then too — neither has to wait for the signature to expire, and neither
	// depends on a revocation list a restart would have lost.
	user, known := s.users[claims.Subject]
	if !known {
		return Identity{}, false
	}
	if _, gone := s.revoked[claims.ID]; gone {
		return Identity{}, false
	}
	if !s.now().Before(claims.expiry()) {
		return Identity{}, false
	}
	return Identity{Name: claims.Subject, Role: user.Role, ExpiresAt: claims.expiry().UTC()}, true
}

// claims is what the token says: who it is for and how long it lives. It deliberately says
// nothing about the role, because a signed claim would freeze what the configuration is the
// source of truth for, and two answers to one question eventually disagree.
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

// cookieSecure marks the cookie Secure when the browser reached the service over TLS: either
// the request itself carries a TLS connection, or a proxy this deployment trusts terminated
// TLS and said so in X-Forwarded-Proto. The second half is the case that matters, because a
// proxy in front of the service is the normal deployment, and without it every session cookie
// would be issued without Secure and could be sent back over plain HTTP.
//
// The header is believed only from a peer in server.trusted_proxies: the same list that says
// whose X-Forwarded-For the limiter counts, so a deployment cannot end up trusting one
// forwarded header and not the other's sender. A client that is not a proxy cannot make its
// cookie Secure by claiming to be one, which would otherwise be a way to have the browser
// drop the session it just received.
func (s *Store) cookieSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if _, peer, ok := peerAddress(r); !ok || !s.trusts(peer) {
		return false
	}
	// A chain of proxies appends to the header; the leftmost value is the one the browser
	// spoke TLS to.
	proto := r.Header.Get("X-Forwarded-Proto")
	if comma := strings.IndexByte(proto, ','); comma >= 0 {
		proto = proto[:comma]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// ClientAddress is the address the sign-in limiter counts and the audit record names: the
// peer that connected, unless
// that peer is a proxy this deployment trusts, in which case it is the client the proxy
// forwarded for. A deployment behind a reverse proxy sees every request from the proxy, so
// keying on the peer alone would let five bad guesses from one person lock out everyone
// behind it.
func (s *Store) ClientAddress(r *http.Request) string {
	remote, peer, ok := peerAddress(r)
	if !ok || !s.trusts(peer) {
		// Whatever a client that is not a trusted proxy claims in the header, it is counted
		// on its own address: otherwise rotating X-Forwarded-For would reset the count.
		return remote
	}

	// X-Forwarded-For is written client first and appended to by each proxy, so the entries
	// nearest this service are the ones most likely to be proxies we run. Walk back over
	// those; the first hop we do not run is the client.
	hops := forwardedFor(r)
	for i := len(hops) - 1; i >= 0; i-- {
		hop, ok := parseAddr(hops[i])
		if !ok {
			// A chain this service cannot read falls back to the peer it can see: being wrong
			// about where a guess came from must not make the limiter disappear.
			return remote
		}
		if !s.trusts(hop) {
			return hop.String()
		}
	}
	if len(hops) > 0 {
		// Every hop is one of ours, so the leftmost entry is the furthest back the chain
		// records, and the closest this service can honestly get to the client.
		first, _ := parseAddr(hops[0])
		return first.String()
	}
	return remote
}

// peerAddress is who connected, without the port: the address a proxy decision has to be
// made about.
func peerAddress(r *http.Request) (string, netip.Addr, bool) {
	host := r.RemoteAddr
	if split, _, err := net.SplitHostPort(host); err == nil {
		host = split
	}
	addr, ok := parseAddr(host)
	return host, addr, ok
}

func (s *Store) trusts(addr netip.Addr) bool {
	for _, prefix := range s.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// forwardedFor is the client chain a proxy appended, in the order it was written: several
// header lines are read in the same order as their comma-separated entries.
func forwardedFor(r *http.Request) []string {
	var hops []string
	for _, header := range r.Header.Values("X-Forwarded-For") {
		for _, hop := range strings.Split(header, ",") {
			if hop = strings.TrimSpace(hop); hop != "" {
				hops = append(hops, hop)
			}
		}
	}
	return hops
}

func parseAddr(text string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(text))
	if err != nil {
		return netip.Addr{}, false
	}
	// An IPv4-mapped IPv6 address is the same client as the IPv4 one, so both spellings must
	// land on one count.
	return addr.Unmap(), true
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
