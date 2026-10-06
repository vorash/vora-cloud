package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vorash/vora-cloud/pkg/githubapp"
)

type fakeGitHub struct {
	mu       sync.Mutex
	linkErr  error
	linked   []linkCall
	installs map[uuid.UUID][]githubapp.Installation
	repos    map[int64]*githubapp.Repositories
	repoErr  map[int64]error
}

type linkCall struct {
	user uuid.UUID
	code string
}

func (f *fakeGitHub) AuthorizeURL(state string) string {
	return "https://github.test/login/oauth/authorize?state=" + url.QueryEscape(state)
}

func (f *fakeGitHub) InstallURL(state string) string {
	return "https://github.test/apps/vora-cloud/installations/new?state=" + url.QueryEscape(state)
}

func (f *fakeGitHub) Link(_ context.Context, user uuid.UUID, code string) (*githubapp.Installation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linked = append(f.linked, linkCall{user, code})
	if f.linkErr != nil {
		return nil, f.linkErr
	}
	return &githubapp.Installation{ID: 1, UserID: user, AccountLogin: "octo"}, nil
}

func (f *fakeGitHub) Installations(_ context.Context, user uuid.UUID) ([]githubapp.Installation, error) {
	return f.installs[user], nil
}

// Repository answers for the installations and repositories in repos, as the
// real one does for the user's own installation only.
func (f *fakeGitHub) Repository(_ context.Context, user uuid.UUID, installationID, repoID int64) (*githubapp.Repository, error) {
	if err := f.repoErr[installationID]; err != nil {
		return nil, err
	}
	for _, in := range f.installs[user] {
		if in.ID != installationID {
			continue
		}
		if f.repos[in.ID] != nil {
			for _, r := range f.repos[in.ID].List {
				if r.ID == repoID {
					return &r, nil
				}
			}
		}
	}
	return nil, githubapp.ErrNotFound
}

func (f *fakeGitHub) Repositories(_ context.Context, _ uuid.UUID, id int64) (*githubapp.Repositories, error) {
	if err := f.repoErr[id]; err != nil {
		return nil, err
	}
	return f.repos[id], nil
}

func newGitHubEnv(t *testing.T) (*env, *fakeGitHub) {
	t.Helper()
	v := newEnv(t)
	gh := &fakeGitHub{installs: map[uuid.UUID][]githubapp.Installation{}, repos: map[int64]*githubapp.Repositories{}, repoErr: map[int64]error{}}
	v.dash.cfg.GitHub = gh
	v.e = echo.New()
	v.dash.Register(v.e)
	return v, gh
}

// stateOf starts a connection as the cookie's user and returns the state GitHub
// would send back.
func stateOf(t *testing.T, v *env, cookie *http.Cookie) string {
	t.Helper()
	r := v.send("GET", "/github/connect", nil, cookie, nil)
	if r.code != http.StatusSeeOther {
		t.Fatalf("connect: %d %s", r.code, r.body)
	}
	loc, err := url.Parse(r.header.Get("Location"))
	if err != nil || loc.Host != "github.test" || loc.Query().Get("state") == "" {
		t.Fatalf("connect redirected to %q", r.header.Get("Location"))
	}
	return loc.Query().Get("state")
}

func finishURL(code, state string) string {
	return "/github/finish?" + url.Values{"code": {code}, "state": {state}}.Encode()
}

func TestGitHubIsOffWithoutTheApp(t *testing.T) {
	v := newEnv(t) // no GitHub configured
	c := v.login(t, "alice@example.com", "alice-pass")
	for _, path := range []string{"/github", "/github/connect", "/github/finish", "/github/callback"} {
		if r := v.send("GET", path, nil, c, nil); r.code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, r.code)
		}
	}
	if body := v.send("GET", "/", nil, c, nil).body; strings.Contains(body, "/github") {
		t.Error("the navigation links to GitHub although it is off")
	}
}

func TestGitHubRoutesNeedASession(t *testing.T) {
	v, gh := newGitHubEnv(t)
	for _, path := range []string{"/github", "/github/connect", finishURL("code", "x.y")} {
		if r := v.send("GET", path, nil, nil, nil); r.code != http.StatusSeeOther || r.header.Get("Location") != "/login" {
			t.Errorf("%s: %d %s", path, r.code, r.header.Get("Location"))
		}
	}
	if len(gh.linked) != 0 {
		t.Error("an anonymous request reached Link")
	}
}

// GitHub sends the user back from another site, so the browser withholds the
// Strict session cookie: this request is anonymous, and must still work. It
// must pass on only the code and the state, whatever else the URL carries.
func TestCallbackIsAnonymousAndPassesOnlyCodeAndState(t *testing.T) {
	v, gh := newGitHubEnv(t)
	q := url.Values{"code": {"c0de"}, "state": {"123.abc"}, "installation_id": {"999"}, "setup_action": {"install"}, "evil": {"1"}}
	r := v.send("GET", "/github/callback?"+q.Encode(), nil, nil, nil)
	if r.code != http.StatusOK {
		t.Fatalf("got %d %s", r.code, r.body)
	}
	want := "/github/finish?code=c0de&state=123.abc"
	if got := r.header.Get("Refresh"); got != "0; url="+want {
		t.Errorf("Refresh = %q", got)
	}
	if !strings.Contains(r.body, `href="`+strings.ReplaceAll(want, "&", "&amp;")+`"`) {
		t.Errorf("the page has no link to %s: %s", want, r.body)
	}
	if strings.Contains(r.body+r.header.Get("Refresh"), "999") || strings.Contains(r.body, "evil") {
		t.Error("a parameter other than code and state was passed on")
	}
	if len(gh.linked) != 0 {
		t.Error("the callback itself linked something")
	}
}

// The values come from the URL and are put in a header and a page.
func TestCallbackEscapesWhatItIsGiven(t *testing.T) {
	v, _ := newGitHubEnv(t)
	hostile := "\r\nSet-Cookie: x=1\"><script>alert(1)</script>"
	r := v.send("GET", "/github/callback?"+url.Values{"code": {hostile}, "state": {hostile}}.Encode(), nil, nil, nil)
	if r.code != http.StatusOK {
		t.Fatalf("got %d", r.code)
	}
	if strings.ContainsAny(r.header.Get("Refresh"), "\r\n") || r.header.Get("Set-Cookie") != "" {
		t.Errorf("header injection: Refresh %q, Set-Cookie %q", r.header.Get("Refresh"), r.header.Get("Set-Cookie"))
	}
	if strings.Contains(r.body, "<script>") || strings.Contains(r.body, `"><script`) {
		t.Errorf("unescaped input in the page: %s", r.body)
	}
}

func TestCallbackAfterTheUserDeniedAccess(t *testing.T) {
	v, _ := newGitHubEnv(t)
	r := v.send("GET", "/github/callback?error=access_denied&state=1.x", nil, nil, nil)
	if r.code != http.StatusBadRequest || r.header.Get("Refresh") != "" {
		t.Errorf("got %d, Refresh %q", r.code, r.header.Get("Refresh"))
	}
}

func TestFinishLinksForTheSessionThatStarted(t *testing.T) {
	v, gh := newGitHubEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	state := stateOf(t, v, c)

	r := v.send("GET", finishURL("the-code", state), nil, c, nil)
	if r.code != http.StatusSeeOther || r.header.Get("Location") != "/github" {
		t.Fatalf("got %d %s %s", r.code, r.header.Get("Location"), r.body)
	}
	if len(gh.linked) != 1 || gh.linked[0] != (linkCall{aliceID, "the-code"}) {
		t.Errorf("Link calls: %+v", gh.linked)
	}
}

// Without this binding, a link someone else built would connect a victim's
// GitHub account to the attacker's Vora account.
func TestFinishRefusesAStateThatIsNotThisSessions(t *testing.T) {
	v, gh := newGitHubEnv(t)
	alice := v.login(t, "alice@example.com", "alice-pass")
	bob := v.login(t, "bob@example.com", "bob-pass")
	good := stateOf(t, v, alice)
	exp, mac, _ := strings.Cut(good, ".")

	for name, tc := range map[string]struct {
		cookie *http.Cookie
		state  string
	}{
		"another session's state": {bob, good},
		"no state":                {alice, ""},
		"not a state":             {alice, "garbage"},
		"a tampered expiry":       {alice, "99999999999." + mac},
		"a tampered mac":          {alice, exp + "." + strings.Repeat("0", len(mac))},
		"a state with no mac":     {alice, exp + "."},
	} {
		t.Run(name, func(t *testing.T) {
			r := v.send("GET", finishURL("code", tc.state), nil, tc.cookie, nil)
			if r.code != http.StatusForbidden {
				t.Errorf("got %d %s, want 403", r.code, r.body)
			}
		})
	}
	if len(gh.linked) != 0 {
		t.Errorf("Link was reached with a bad state: %+v", gh.linked)
	}
}

// GitHub's code lives ten minutes, and so does the state.
func TestFinishRefusesAnExpiredState(t *testing.T) {
	v, gh := newGitHubEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	state := stateOf(t, v, c)

	v.dash.now = func() time.Time { return time.Now().Add(stateLifetime - time.Minute) }
	if r := v.send("GET", finishURL("code", state), nil, c, nil); r.code != http.StatusSeeOther {
		t.Errorf("a state with a minute left: %d", r.code)
	}
	v.dash.now = func() time.Time { return time.Now().Add(stateLifetime + time.Minute) }
	if r := v.send("GET", finishURL("code", state), nil, c, nil); r.code != http.StatusForbidden {
		t.Errorf("an expired state: %d, want 403", r.code)
	}
	if len(gh.linked) != 1 {
		t.Errorf("Link calls: %d, want only the first", len(gh.linked))
	}
}

func TestFinishExplainsWhyItFailed(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		code    int
		message string
		install bool
	}{
		"code refused":      {githubapp.ErrAuthorization, http.StatusBadRequest, "did not accept the authorization", false},
		"not installed":     {githubapp.ErrNotInstalled, http.StatusUnprocessableEntity, "not installed on your personal GitHub account", true},
		"taken by another":  {githubapp.ErrInstallationTaken, http.StatusConflict, "already connected to another Vora account", false},
		"something unusual": {errors.New("db password is hunter2"), http.StatusInternalServerError, "Try again in a moment", false},
	} {
		t.Run(name, func(t *testing.T) {
			v, gh := newGitHubEnv(t)
			gh.linkErr = tc.err
			c := v.login(t, "alice@example.com", "alice-pass")
			r := v.send("GET", finishURL("code", stateOf(t, v, c)), nil, c, nil)
			if r.code != tc.code || !strings.Contains(r.body, tc.message) {
				t.Errorf("got %d %s", r.code, r.body)
			}
			if hasInstall := strings.Contains(r.body, "github.test/apps/vora-cloud/installations/new"); hasInstall != tc.install {
				t.Errorf("install link present = %v, want %v", hasInstall, tc.install)
			}
			if strings.Contains(r.body, "hunter2") {
				t.Error("an internal error reached the page")
			}
		})
	}
}

// The link GitHub sends users to for installing carries a state of its own,
// so coming back from it works the same way.
func TestInstallLinkCarriesAStateThatFinishAccepts(t *testing.T) {
	v, gh := newGitHubEnv(t)
	gh.linkErr = githubapp.ErrNotInstalled
	c := v.login(t, "alice@example.com", "alice-pass")
	body := v.send("GET", finishURL("code", stateOf(t, v, c)), nil, c, nil).body
	m := regexpFind(t, `installations/new\?state=([^"]+)"`, body)
	state, err := url.QueryUnescape(strings.ReplaceAll(m, "&amp;", "&"))
	if err != nil {
		t.Fatal(err)
	}
	gh.linkErr = nil
	if r := v.send("GET", finishURL("code", state), nil, c, nil); r.code != http.StatusSeeOther {
		t.Errorf("finish with the install link's state: %d", r.code)
	}
}

func TestGitHubPageListsOnlyTheUsersOwnRepositories(t *testing.T) {
	v, gh := newGitHubEnv(t)
	gh.installs[aliceID] = []githubapp.Installation{{ID: 1, UserID: aliceID, AccountLogin: "octo"}}
	gh.installs[bobID] = []githubapp.Installation{{ID: 2, UserID: bobID, AccountLogin: "bobby"}}
	gh.repos[1] = &githubapp.Repositories{Total: 205, List: []githubapp.Repository{
		{FullName: "octo/web", Private: true, DefaultBranch: "main"},
		{FullName: "octo/<b>docs</b>", DefaultBranch: "trunk"},
	}}
	gh.repos[2] = &githubapp.Repositories{Total: 1, List: []githubapp.Repository{{FullName: "bobby/secret", Private: true, DefaultBranch: "main"}}}
	c := v.login(t, "alice@example.com", "alice-pass")

	r := v.send("GET", "/github", nil, c, nil)
	if r.code != http.StatusOK {
		t.Fatalf("got %d", r.code)
	}
	for _, want := range []string{"octo/web", "private · main", "public · trunk", "Showing 2 of 205"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(r.body, "bobby") || strings.Contains(r.body, "<b>docs</b>") {
		t.Errorf("another user's repositories, or unescaped names, on the page: %s", r.body)
	}
	// A way out when the connection has gone stale (the app uninstalled on GitHub).
	for _, want := range []string{`href="https://github.com/settings/installations/1"`, `href="/github/connect"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("a connected user has no %s", want)
		}
	}
	if strings.Contains(r.body, "installations/2") {
		t.Error("a link to another user's installation")
	}
	if !strings.Contains(r.body, `href="/github" aria-current="page"`) {
		t.Error("the navigation does not mark the GitHub page")
	}
}

func TestGitHubPageSurvivesGitHubFailing(t *testing.T) {
	v, gh := newGitHubEnv(t)
	gh.installs[aliceID] = []githubapp.Installation{{ID: 1, UserID: aliceID, AccountLogin: "octo"}}
	gh.repoErr[1] = errors.New("502 from api.github.com, token ghs_secret")
	c := v.login(t, "alice@example.com", "alice-pass")

	r := v.send("GET", "/github", nil, c, nil)
	if r.code != http.StatusOK || !strings.Contains(r.body, "could not be loaded") || strings.Contains(r.body, "ghs_secret") {
		t.Errorf("got %d %s", r.code, r.body)
	}
}

func TestGitHubPageWithNothingConnected(t *testing.T) {
	v, _ := newGitHubEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	r := v.send("GET", "/github", nil, c, nil)
	if r.code != http.StatusOK || !strings.Contains(r.body, `href="/github/connect"`) {
		t.Errorf("got %d %s", r.code, r.body)
	}
}

func regexpFind(t *testing.T, pattern, s string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no match for %s in %s", pattern, s)
	}
	return m[1]
}
