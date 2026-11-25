package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// CSRFCookie carries the double-submit token. It is deliberately *not* HttpOnly: the dashboard
// has to read it to echo it back, which is the whole point of the pattern. The cookie is
// SameSite=Lax like the session, so it is not sent on another site's request either.
const CSRFCookie = "gohorizon_csrf"

// CSRFHeader is the header a mutation must carry, repeating the cookie's value.
const CSRFHeader = "X-CSRF-Token"

// IssueCSRF sets a fresh double-submit token. It is called when a session starts and whenever
// the session route is read without one, so a browser that signed in before mutations existed
// picks a token up without signing in again.
func (s *Store) IssueCSRF(w http.ResponseWriter, r *http.Request) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		// A predictable token would be no token at all, and a forged one would pass the
		// check. Leaving the cookie unset makes the next mutation fail its check instead,
		// which is the safe way for a random source to break.
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookie,
		Value:    base64.RawURLEncoding.EncodeToString(token),
		Path:     "/",
		HttpOnly: false,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.ttl.Seconds()),
	})
}

// VerifyCSRF reports whether the request echoes the token its cookie carries. Both halves come
// from the client, so this does not prove the value is secret — it proves the request was made
// by something that can read this origin's cookies and set a header, which a form on another
// site can do neither of. The comparison is constant time so a wrong token cannot be narrowed
// down a byte at a time.
func (s *Store) VerifyCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(CSRFCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	presented := r.Header.Get(CSRFHeader)
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(cookie.Value)) == 1
}
