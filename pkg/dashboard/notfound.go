package dashboard

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// brandNotFound makes the router answer a browser's request for an unknown URL
// with the dashboard's 404 page instead of echo's JSON. The error handler is
// shared with the REST API, so it only takes over GET requests that accept
// HTML and are not under /api/ or /swagger/; every other error, and every
// other client, keeps the handler that was there.
func (d *Dashboard) brandNotFound(e *echo.Echo) {
	prev := e.HTTPErrorHandler
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		var he *echo.HTTPError
		if errors.As(err, &he) && he.Code == http.StatusNotFound && wantsPage(c.Request()) && !c.Response().Committed {
			d.softSession(c)
			// An unknown route skips route middleware, so the headers are set here.
			if d.secureHeaders(d.notFound)(c) == nil {
				return
			}
		}
		prev(err, c)
	}
}

func wantsPage(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") &&
		!strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/swagger/")
}

// softSession loads the session behind the cookie, if there is one, only so the
// page can show the navigation and the name. It changes nothing: a missing or
// expired session is not an error here, and no cookie is cleared.
func (d *Dashboard) softSession(c echo.Context) {
	cookie, err := c.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		return
	}
	hash := hashToken(cookie.Value)
	sess, err := d.cfg.Sessions.Get(c.Request().Context(), hash)
	if err != nil || sess == nil {
		return
	}
	c.Set(sessionKey, sess)
	c.Set(csrfKey, d.csrfFor(hash))
}
