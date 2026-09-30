package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A mistyped URL in a browser gets the dashboard's page, with the navigation if
// the person is signed in and the security headers either way.
func TestUnknownURLInABrowserGetsTheDashboardPage(t *testing.T) {
	v := newEnv(t)
	html := map[string]string{"Accept": "text/html,application/xhtml+xml"}

	anon := v.send("GET", "/nowhere", nil, nil, html)
	if anon.code != http.StatusNotFound || !strings.Contains(anon.body, "Page not found") || strings.Contains(anon.body, `class="rail"`) {
		t.Errorf("signed out: %d %s", anon.code, anon.body)
	}
	if !strings.Contains(anon.header.Get("Content-Security-Policy"), "frame-ancestors 'none'") || anon.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("the 404 page must carry the security headers: %v", anon.header)
	}

	alice := v.login(t, "alice@example.com", "alice-pass")
	in := v.send("GET", "/projects/x/y/z", nil, alice, html)
	if in.code != http.StatusNotFound || !strings.Contains(in.body, `class="rail"`) || !strings.Contains(in.body, "Alice") {
		t.Errorf("signed in: %d %s", in.code, in.body)
	}
	expired := *alice
	expired.Value = "not-a-session"
	if r := v.send("GET", "/nowhere", nil, &expired, html); r.code != http.StatusNotFound || strings.Contains(r.body, `class="rail"`) || len(r.cookies) != 0 {
		t.Errorf("an unknown session only means signed out here: %d, %d cookies", r.code, len(r.cookies))
	}
}

// The REST API shares the router: its clients, its paths and its other errors
// keep echo's JSON.
func TestOtherClientsAndErrorsKeepTheJSONHandler(t *testing.T) {
	v := newEnv(t)
	html := map[string]string{"Accept": "text/html"}
	cases := []struct {
		name, method, path string
		headers            map[string]string
		want               int
	}{
		{"no Accept header", "GET", "/nowhere", nil, http.StatusNotFound},
		{"a JSON client", "GET", "/nowhere", map[string]string{"Accept": "application/json"}, http.StatusNotFound},
		{"under /api", "GET", "/api/v1/nope", html, http.StatusNotFound},
		{"under /swagger", "GET", "/swagger/nope", html, http.StatusNotFound},
		{"a write", "POST", "/nowhere", html, http.StatusNotFound},
		{"a wrong method is not a 404", "GET", "/logout", html, http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		var form url.Values
		if c.method == "POST" {
			form = url.Values{}
		}
		r := v.send(c.method, c.path, form, nil, c.headers)
		if r.code != c.want || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") || strings.Contains(r.body, "<html") {
			t.Errorf("%s: %d %q %s", c.name, r.code, r.header.Get("Content-Type"), r.body)
		}
	}
}
