package dashboard

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vorash/vora-cloud/pkg/auth"
	"github.com/vorash/vora-cloud/pkg/database"
	"github.com/vorash/vora-cloud/pkg/deploy"
	"github.com/vorash/vora-cloud/pkg/deploy/k3s"
	"github.com/vorash/vora-cloud/pkg/project"
	"github.com/vorash/vora-cloud/pkg/project/projecttest"
	"github.com/vorash/vora-cloud/pkg/secrets"
)

const site = "https://cloud.test"

type fakeSessions struct {
	mu   sync.Mutex
	rows map[string]*database.Session // by token hash
}

func (f *fakeSessions) Create(_ context.Context, id uuid.UUID, hash string, exp time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[hash] = &database.Session{UserID: id, UserName: users[id], ExpiresAt: exp}
	return nil
}

func (f *fakeSessions) Get(_ context.Context, hash string) (*database.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.rows[hash]; s != nil && s.ExpiresAt.After(time.Now()) {
		return s, nil
	}
	return nil, nil
}

func (f *fakeSessions) Delete(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, hash)
	return nil
}

var (
	aliceID = uuid.New()
	bobID   = uuid.New()
	users   = map[uuid.UUID]string{aliceID: "Alice", bobID: "Bob"}
)

type fakeUsers struct{}

func (fakeUsers) AuthenticateUser(_ context.Context, email, password string) (*database.User, error) {
	switch {
	case email == "alice@example.com" && password == "alice-pass":
		return &database.User{ID: aliceID, Name: "Alice", Email: email}, nil
	case email == "bob@example.com" && password == "bob-pass":
		return &database.User{ID: bobID, Name: "Bob", Email: email}, nil
	}
	return nil, auth.ErrInvalidCredentials
}

type env struct {
	e        *echo.Echo
	sessions *fakeSessions
	projects *project.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	reg, err := deploy.NewRegistry(map[string]deploy.Deployer{"eu": k3s.New(fake.NewClientset(), k3s.Options{})})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := project.NewService(projecttest.New(), reg, box, "cloud.example.com")
	ss := &fakeSessions{rows: map[string]*database.Session{}}
	d, err := New(Config{
		Sessions: ss, Users: fakeUsers{}, Projects: svc, Regions: reg.Regions(),
		PublicURL: site, CSRFKey: bytes.Repeat([]byte{7}, 32),
		Limiter: auth.NewAttemptLimiter(12*time.Second, 5),
	})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	d.Register(e)
	return &env{e: e, sessions: ss, projects: svc}
}

type resp struct {
	code    int
	body    string
	header  http.Header
	cookies []*http.Cookie
}

// send makes a request. Writes carry the site's Origin unless headers say otherwise.
func (v *env) send(method, path string, form url.Values, cookie *http.Cookie, headers map[string]string) resp {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method != http.MethodGet {
		req.Header.Set("Origin", site)
	}
	for k, val := range headers {
		if val == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, val)
		}
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Body.String(), rec.Header(), rec.Result().Cookies()}
}

func (v *env) login(t *testing.T, email, password string) *http.Cookie {
	t.Helper()
	r := v.send("POST", "/login", url.Values{"email": {email}, "password": {password}}, nil, nil)
	if r.code != http.StatusSeeOther {
		t.Fatalf("login %s: %d %s", email, r.code, r.body)
	}
	for _, c := range r.cookies {
		if c.Name == cookieName && c.Value != "" {
			return c
		}
	}
	t.Fatal("login set no session cookie")
	return nil
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// csrf reads the form token a signed-in page carries.
func (v *env) csrf(t *testing.T, cookie *http.Cookie) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(v.send("GET", "/projects/new", nil, cookie, nil).body)
	if m == nil {
		t.Fatal("no csrf token on the page")
	}
	return m[1]
}

func (v *env) createProject(t *testing.T, cookie *http.Cookie, name, slug string) string {
	t.Helper()
	r := v.send("POST", "/projects", url.Values{"csrf": {v.csrf(t, cookie)}, "name": {name}, "slug": {slug}, "region": {"eu"}}, cookie, nil)
	if r.code != http.StatusSeeOther || !strings.HasPrefix(r.header.Get("Location"), "/projects/") {
		t.Fatalf("create project: %d %s", r.code, r.body)
	}
	return r.header.Get("Location")
}

func TestAnonymousVisitorsAreSentToSignIn(t *testing.T) {
	v := newEnv(t)
	for _, path := range []string{"/", "/projects/new", "/projects/" + uuid.NewString()} {
		if r := v.send("GET", path, nil, nil, nil); r.code != http.StatusSeeOther || r.header.Get("Location") != "/login" {
			t.Errorf("%s: %d %s", path, r.code, r.header.Get("Location"))
		}
	}
	for _, path := range []string{"/logout", "/projects"} {
		if r := v.send("POST", path, url.Values{}, nil, nil); r.code != http.StatusSeeOther || r.header.Get("Location") != "/login" {
			t.Errorf("POST %s: %d", path, r.code)
		}
	}
}

// An unknown email and a wrong password must look the same, or the form tells
// an attacker which accounts exist.
func TestSignInFailuresAreIndistinguishable(t *testing.T) {
	v := newEnv(t)
	wrong := v.send("POST", "/login", url.Values{"email": {"alice@example.com"}, "password": {"nope"}}, nil, nil)
	unknown := v.send("POST", "/login", url.Values{"email": {"nobody@example.com"}, "password": {"nope"}}, nil, nil)
	if wrong.code != http.StatusUnauthorized || unknown.code != wrong.code || unknown.body != wrong.body {
		t.Errorf("wrong password: %d, unknown email: %d, same body: %v", wrong.code, unknown.code, unknown.body == wrong.body)
	}
	if len(v.sessions.rows) != 0 {
		t.Error("a failed sign-in created a session")
	}
}

func TestSessionCookieIsLockedDownAndStoredHashed(t *testing.T) {
	v := newEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
		t.Errorf("cookie attributes: %+v", c)
	}
	if !strings.HasPrefix(c.Name, "__Host-") {
		t.Errorf("cookie %q lacks the __Host- prefix", c.Name)
	}
	if _, stored := v.sessions.rows[c.Value]; stored {
		t.Error("the raw token is the store key; a database dump could be replayed")
	}
	if _, stored := v.sessions.rows[hashToken(c.Value)]; !stored {
		t.Error("the session is not stored under the token's hash")
	}
}

// Every write must come from the site's own pages. A missing or foreign Origin
// is refused, including on sign-in, where there is no session token yet.
func TestWritesFromOtherOriginsAreRefused(t *testing.T) {
	v := newEnv(t)
	form := url.Values{"email": {"alice@example.com"}, "password": {"alice-pass"}}
	for name, h := range map[string]map[string]string{
		"no origin":        {"Origin": ""},
		"foreign origin":   {"Origin": "https://evil.example"},
		"null origin":      {"Origin": "null"},
		"http downgrade":   {"Origin": "http://cloud.test"},
		"cross-site fetch": {"Sec-Fetch-Site": "cross-site"},
	} {
		if r := v.send("POST", "/login", form, nil, h); r.code != http.StatusForbidden {
			t.Errorf("sign-in with %s: %d, want 403", name, r.code)
		}
	}
	if len(v.sessions.rows) != 0 {
		t.Error("a refused sign-in created a session")
	}

	c := v.login(t, "alice@example.com", "alice-pass")
	tok := v.csrf(t, c)
	r := v.send("POST", "/projects", url.Values{"csrf": {tok}, "name": {"x"}, "slug": {"x"}, "region": {"eu"}}, c, map[string]string{"Origin": "https://evil.example"})
	if r.code != http.StatusForbidden {
		t.Errorf("create project from another origin: %d, want 403", r.code)
	}
}

func TestWritesNeedTheSessionsFormToken(t *testing.T) {
	v := newEnv(t)
	alice := v.login(t, "alice@example.com", "alice-pass")
	bob := v.login(t, "bob@example.com", "bob-pass")
	bobToken := v.csrf(t, bob)
	form := func(tok string) url.Values {
		return url.Values{"csrf": {tok}, "name": {"Web"}, "slug": {"web"}, "region": {"eu"}}
	}
	for name, tok := range map[string]string{"missing": "", "invented": "abc", "another session's": bobToken} {
		if r := v.send("POST", "/projects", form(tok), alice, nil); r.code != http.StatusForbidden {
			t.Errorf("%s token: %d, want 403", name, r.code)
		}
	}
	if list, _ := v.projects.ListProjects(context.Background(), aliceID); len(list) != 0 {
		t.Errorf("a refused write created %d projects", len(list))
	}
	if r := v.send("POST", "/projects", form(v.csrf(t, alice)), alice, nil); r.code != http.StatusSeeOther {
		t.Errorf("with the right token: %d", r.code)
	}
}

// The API's ownership rule holds in the UI: another user's project is "not found".
func TestAnotherUsersProjectIsNotFound(t *testing.T) {
	v := newEnv(t)
	alice := v.login(t, "alice@example.com", "alice-pass")
	bob := v.login(t, "bob@example.com", "bob-pass")
	page := v.createProject(t, alice, "Web", "web")
	id := strings.TrimPrefix(page, "/projects/")

	if r := v.send("GET", page, nil, bob, nil); r.code != http.StatusNotFound || strings.Contains(r.body, "Web") {
		t.Errorf("bob viewing: %d", r.code)
	}
	bt := v.csrf(t, bob)
	for _, path := range []string{page + "/deployments", page + "/redeploy"} {
		if r := v.send("POST", path, url.Values{"csrf": {bt}, "image": {"evil:1"}}, bob, nil); r.code != http.StatusNotFound {
			t.Errorf("bob POST %s: %d, want 404", path, r.code)
		}
	}
	if r := v.send("GET", "/projects/not-a-uuid", nil, bob, nil); r.code != http.StatusNotFound {
		t.Errorf("malformed id: %d", r.code)
	}
	if deps, _ := v.projects.ListDeployments(context.Background(), aliceID, uuid.MustParse(id)); len(deps) != 0 {
		t.Errorf("bob's request deployed to alice's project: %v", deps)
	}
	if r := v.send("GET", "/", nil, bob, nil); strings.Contains(r.body, "Web") {
		t.Error("bob's list shows alice's project")
	}
}

func TestDeployFromTheProjectPage(t *testing.T) {
	v := newEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	page := v.createProject(t, c, "Web", "web")
	tok := v.csrf(t, c)

	if r := v.send("POST", page+"/redeploy", url.Values{"csrf": {tok}}, c, nil); r.code != http.StatusUnprocessableEntity || !strings.Contains(r.body, "no deployment") {
		t.Errorf("redeploy before any deploy: %d %s", r.code, r.body)
	}
	if r := v.send("POST", page+"/deployments", url.Values{"csrf": {tok}, "image": {"reg/web:1"}}, c, nil); r.code != http.StatusSeeOther || r.header.Get("Location") != page {
		t.Fatalf("deploy: %d", r.code)
	}
	body := v.send("GET", page, nil, c, nil).body
	for _, want := range []string{"deploying", "reg/web:1", "https://web.eu.cloud.example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("project page lacks %q", want)
		}
	}
}

func TestCreateProjectShowsWhyItWasRefused(t *testing.T) {
	v := newEnv(t)
	alice := v.login(t, "alice@example.com", "alice-pass")
	bob := v.login(t, "bob@example.com", "bob-pass")
	v.createProject(t, alice, "Web", "web")
	for name, slug := range map[string]string{"invalid": "Bad_Slug", "taken": "web"} {
		r := v.send("POST", "/projects", url.Values{"csrf": {v.csrf(t, bob)}, "name": {"X"}, "slug": {slug}, "region": {"eu"}}, bob, nil)
		if r.code != http.StatusUnprocessableEntity || !strings.Contains(r.body, `role="alert"`) || !strings.Contains(r.body, slug) {
			t.Errorf("%s slug: %d", name, r.code)
		}
	}
}

// User-chosen text must reach the page escaped.
func TestUserTextIsEscaped(t *testing.T) {
	v := newEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	page := v.createProject(t, c, `<script>alert(1)</script>`, "web")
	for _, path := range []string{"/", page} {
		body := v.send("GET", path, nil, c, nil).body
		if strings.Contains(body, "<script>alert(1)") || !strings.Contains(body, "&lt;script&gt;") {
			t.Errorf("%s does not escape the project name", path)
		}
	}
}

func TestSignOutEndsTheSession(t *testing.T) {
	v := newEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	if r := v.send("POST", "/logout", url.Values{"csrf": {v.csrf(t, c)}}, c, nil); r.code != http.StatusSeeOther {
		t.Fatalf("logout: %d", r.code)
	}
	if r := v.send("GET", "/", nil, c, nil); r.code != http.StatusSeeOther || r.header.Get("Location") != "/login" {
		t.Errorf("the old cookie still works: %d", r.code)
	}
	if len(v.sessions.rows) != 0 {
		t.Error("the session row was not deleted")
	}
}

func TestExpiredSessionIsRefused(t *testing.T) {
	v := newEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	for _, s := range v.sessions.rows {
		s.ExpiresAt = time.Now().Add(-time.Minute)
	}
	if r := v.send("GET", "/", nil, c, nil); r.code != http.StatusSeeOther {
		t.Errorf("expired session: %d", r.code)
	}
}

func TestSignInIsRateLimitedPerAccount(t *testing.T) {
	v := newEnv(t)
	guess := func(email string) int {
		return v.send("POST", "/login", url.Values{"email": {email}, "password": {"nope"}}, nil, nil).code
	}
	var last int
	for i := 0; i < 8; i++ {
		last = guess("alice@example.com")
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("after 8 guesses at one account: %d, want 429", last)
	}
	if got := guess("bob@example.com"); got != http.StatusUnauthorized {
		t.Errorf("another account: %d, want a normal 401", got)
	}
}

func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	v := newEnv(t)
	for _, path := range []string{"/login", "/assets/app.css", "/"} {
		h := v.send("GET", path, nil, nil, nil).header
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s CSP: %q", path, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s headers: %v", path, h)
		}
	}
	if v.send("GET", "/login", nil, nil, nil).header.Get("Cache-Control") != "no-store" {
		t.Error("pages must not be cached")
	}
}

func TestNewRefusesAWeakConfig(t *testing.T) {
	if _, err := New(Config{CSRFKey: []byte("short"), Limiter: auth.NewAttemptLimiter(time.Second, 1)}); err == nil {
		t.Error("accepted a short CSRF key")
	}
	if _, err := New(Config{CSRFKey: bytes.Repeat([]byte{1}, 32)}); err == nil {
		t.Error("accepted a missing sign-in limiter")
	}
}

// Unknown URLs must stay plain 404s: dashboard middleware must not answer for
// paths the dashboard does not own.
func TestUnknownPathsAre404NotASignInRedirect(t *testing.T) {
	v := newEnv(t)
	for _, path := range []string{"/nope", "/api/v1/nope", "/projects/x/y/z"} {
		if r := v.send("GET", path, nil, nil, nil); r.code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, r.code)
		}
	}
}

func TestOversizedBodiesAreRefused(t *testing.T) {
	v := newEnv(t)
	big := url.Values{"email": {"alice@example.com"}, "password": {strings.Repeat("x", 200_000)}}
	if r := v.send("POST", "/login", big, nil, nil); r.code != http.StatusRequestEntityTooLarge {
		t.Errorf("a 200 KB sign-in body: %d, want 413", r.code)
	}
}
