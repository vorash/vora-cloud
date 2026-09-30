package dashboard

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/vorash/vora-cloud/pkg/database"
)

// The __Host- prefix makes browsers accept the cookie only with Secure, Path=/
// and no Domain, so a sibling subdomain cannot set or shadow it.
const cookieName = "__Host-vora_session"

const (
	sessionKey = "dashboard_session"
	csrfKey    = "dashboard_csrf"
)

// secureHeaders applies to every dashboard response. The pages use no
// scripts, so the policy forbids them, along with framing and third parties.
func (d *Dashboard) secureHeaders(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		h := c.Response().Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		// Not "no-referrer": the Fetch spec then makes browsers send "Origin: null"
		// on form posts even to this same site, and Firefox does, which the
		// Origin check would refuse. "same-origin" sends nothing cross-site.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		if c.Scheme() == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		return next(c)
	}
}

// sameOrigin reports whether a write comes from this site's own pages. Browsers
// send Origin on every POST, so a request without it is refused.
func (d *Dashboard) sameOrigin(c echo.Context) bool {
	if site := c.Request().Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	want := d.cfg.PublicURL
	if want == "" {
		want = c.Scheme() + "://" + c.Request().Host
	}
	origin := c.Request().Header.Get("Origin")
	if origin == "" || origin != want {
		log.Printf("dashboard: refused %s %s: Origin %q, want %q", c.Request().Method, c.Path(), origin, want)
		return false
	}
	return true
}

func (d *Dashboard) originCheck(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !d.sameOrigin(c) {
			return d.errorPage(c, http.StatusForbidden, "Request refused: it did not come from this site.")
		}
		return next(c)
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newToken returns a random 256-bit session token.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// csrfFor derives the form token of a session, so no extra state is stored.
func (d *Dashboard) csrfFor(tokenHash string) string {
	mac := hmac.New(sha256.New, d.cfg.CSRFKey)
	mac.Write([]byte(tokenHash))
	return hex.EncodeToString(mac.Sum(nil))
}

func (d *Dashboard) setSessionCookie(c echo.Context, token string) {
	c.SetCookie(&http.Cookie{
		Name: cookieName, Value: token, Path: "/",
		MaxAge:   int(sessionLifetime / time.Second),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(c echo.Context) {
	c.SetCookie(&http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// requireSession loads the session behind the cookie. Anonymous requests go
// to the sign-in page. Writes must also come from this site and carry the
// session's form token.
func (d *Dashboard) requireSession(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		cookie, err := c.Cookie(cookieName)
		if err != nil || cookie.Value == "" {
			return c.Redirect(http.StatusSeeOther, "/login")
		}
		hash := hashToken(cookie.Value)
		sess, err := d.cfg.Sessions.Get(c.Request().Context(), hash)
		if err != nil {
			return d.serverError(c, err)
		}
		if sess == nil {
			clearSessionCookie(c)
			return c.Redirect(http.StatusSeeOther, "/login")
		}
		csrf := d.csrfFor(hash)
		if c.Request().Method != http.MethodGet {
			if !d.sameOrigin(c) {
				return d.errorPage(c, http.StatusForbidden, "Request refused: it did not come from this site.")
			}
			if !hmac.Equal([]byte(c.FormValue("csrf")), []byte(csrf)) {
				return d.errorPage(c, http.StatusForbidden, "Request refused: the form has expired. Reload the page and try again.")
			}
		}
		c.Set(sessionKey, sess)
		c.Set(csrfKey, csrf)
		return next(c)
	}
}

func sessionOf(c echo.Context) *database.Session { return c.Get(sessionKey).(*database.Session) }
