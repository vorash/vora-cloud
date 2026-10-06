package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/google/uuid"
)

const (
	clientSecret = "oauth-client-secret"
	userToken    = "ghu_usertoken"
	callbackURL  = "https://cloud.test/github/callback"
)

// memLinks is Links with the rules of the real store: an installation belongs
// to the first user who links it.
type memLinks struct {
	mu   sync.Mutex
	rows map[int64]Installation
}

func (m *memLinks) Link(_ context.Context, i Installation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if have, ok := m.rows[i.ID]; ok && have.UserID != i.UserID {
		return ErrInstallationTaken
	}
	m.rows[i.ID] = i
	return nil
}

func (m *memLinks) List(_ context.Context, u uuid.UUID) ([]Installation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Installation
	for _, i := range m.rows {
		if i.UserID == u {
			out = append(out, i)
		}
	}
	return out, nil
}

func (m *memLinks) Get(_ context.Context, u uuid.UUID, id int64) (*Installation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i, ok := m.rows[id]; ok && i.UserID == u {
		return &i, nil
	}
	return nil, ErrNotFound
}

// fakeConnectGitHub is the parts of github.com and api.github.com the connect flow
// talks to. The GitHub user is #7, "octo".
type fakeConnectGitHub struct {
	mu            sync.Mutex
	codeOK        bool
	installations []map[string]any // what GET /user/installations returns
	repos         []map[string]any
	totalRepos    int
	tokenForm     url.Values // the last code exchange request
	tokenRequests int        // how many code exchanges were attempted
	userCalls     []string   // bearer tokens seen on user endpoints
	installTokens int        // installation tokens minted
	pageSize      int        // repositories per page; 0 returns them all at once
	repoPages     int        // pages of the installation's repositories served
}

func account(id int, login, typ string) map[string]any {
	return map[string]any{"id": id, "login": login, "type": typ}
}

func installation(id int, acc map[string]any) map[string]any {
	return map[string]any{"id": id, "account": acc, "target_type": acc["type"]}
}

func (f *fakeConnectGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case r.URL.Path == "/login/oauth/access_token":
		_ = r.ParseForm()
		f.tokenForm = r.PostForm
		f.tokenRequests++
		// GitHub answers a bad code with 200 and an error in the body.
		if !f.codeOK || r.PostForm.Get("client_secret") != clientSecret {
			_, _ = w.Write([]byte(`{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"token_type":"bearer","scope":""}`, userToken)
	case r.URL.Path == "/user":
		f.userCalls = append(f.userCalls, bearer)
		_ = json.NewEncoder(w).Encode(account(7, "octo", "User"))
	case r.URL.Path == "/user/installations":
		f.userCalls = append(f.userCalls, bearer)
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(f.installations), "installations": f.installations})
	case strings.HasSuffix(r.URL.Path, "/access_tokens") && r.Method == http.MethodPost:
		f.installTokens++
		_, _ = fmt.Fprintf(w, `{"token":"ghs_install","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	case r.URL.Path == "/installation/repositories":
		if bearer != "ghs_install" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		f.repoPages++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		list := f.repos
		if f.pageSize > 0 {
			from := min((page-1)*f.pageSize, len(f.repos))
			to := min(from+f.pageSize, len(f.repos))
			list = f.repos[from:to]
			if to < len(f.repos) {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s/installation/repositories?page=%d>; rel="next"`, r.Host, page+1))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": f.totalRepos, "repositories": list})
	case strings.HasPrefix(r.URL.Path, "/repositories/"):
		// Any token can read a public repository by id, the installation's or not.
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 999, "full_name": "stranger/public-repo", "private": false, "default_branch": "main"})
	default:
		http.NotFound(w, r)
	}
}

type connectEnv struct {
	c     *Connector
	gh    *fakeConnectGitHub
	links *memLinks
	alice uuid.UUID
}

func newConnectEnv(t *testing.T) *connectEnv {
	t.Helper()
	_, keyPEM := testKey(t)
	gh := &fakeConnectGitHub{codeOK: true}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	app, err := newApp(clientID, keyPEM, time.Now, github.WithURLs(&base, nil))
	if err != nil {
		t.Fatal(err)
	}
	links := &memLinks{rows: map[int64]Installation{}}
	c := NewConnector(app, links, "vora-cloud", clientID, clientSecret, callbackURL)
	c.oauth.Endpoint.TokenURL = srv.URL + "/login/oauth/access_token"
	return &connectEnv{c: c, gh: gh, links: links, alice: uuid.New()}
}

func TestLinkConnectsTheInstallationOnTheUsersOwnAccount(t *testing.T) {
	e := newConnectEnv(t)
	e.gh.installations = []map[string]any{
		installation(11, account(900, "some-org", "Organization")),
		installation(42, account(7, "octo", "User")),
	}
	got, err := e.c.Link(t.Context(), e.alice, "the-code")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 42 || got.AccountID != 7 || got.AccountLogin != "octo" || got.UserID != e.alice {
		t.Errorf("linked %+v", got)
	}
	if _, ok := e.links.rows[11]; ok {
		t.Error("the organization's installation was linked too")
	}
	// The exchange is the documented one, with the secret in the body.
	f := e.gh.tokenForm
	if f.Get("code") != "the-code" || f.Get("client_id") != clientID || f.Get("client_secret") != clientSecret {
		t.Errorf("token request form: %v", f)
	}
	// One request: the secret was sent in the body at once, not first as a
	// Basic header that GitHub would not take and then again.
	if e.gh.tokenRequests != 1 {
		t.Errorf("%d code exchange requests, want 1", e.gh.tokenRequests)
	}
	// Only the user's own token was used on the user endpoints.
	for _, b := range e.gh.userCalls {
		if b != userToken {
			t.Errorf("a user endpoint was called with %q", b)
		}
	}
}

// The user can list installations they can see, such as an organization's or
// another person's. Linking one would let them read repositories they have no
// right to, so only the installation on their own account counts.
func TestLinkRefusesInstallationsThatAreNotTheUsersOwnAccount(t *testing.T) {
	for name, installs := range map[string][]map[string]any{
		"an organization":                    {installation(11, account(900, "some-org", "Organization"))},
		"another user's account":             {installation(12, account(8, "someone-else", "User"))},
		"an account that reuses the login":   {installation(13, account(99, "octo", "User"))},
		"an organization with the user's id": {installation(14, account(7, "octo", "Organization"))},
		"nothing installed":                  nil,
	} {
		t.Run(name, func(t *testing.T) {
			e := newConnectEnv(t)
			e.gh.installations = installs
			if _, err := e.c.Link(t.Context(), e.alice, "code"); err != ErrNotInstalled {
				t.Errorf("err = %v, want ErrNotInstalled", err)
			}
			if len(e.links.rows) != 0 {
				t.Errorf("linked %v", e.links.rows)
			}
		})
	}
}

// Someone else's installation must never move to another Vora account.
func TestLinkRefusesAnInstallationOfAnotherVoraUser(t *testing.T) {
	e := newConnectEnv(t)
	e.gh.installations = []map[string]any{installation(42, account(7, "octo", "User"))}
	if _, err := e.c.Link(t.Context(), e.alice, "code"); err != nil {
		t.Fatal(err)
	}
	mallory := uuid.New()
	if _, err := e.c.Link(t.Context(), mallory, "code"); err != ErrInstallationTaken {
		t.Errorf("err = %v, want ErrInstallationTaken", err)
	}
	if e.links.rows[42].UserID != e.alice {
		t.Error("the installation changed owner")
	}
	// The owner connecting again is not an error.
	if _, err := e.c.Link(t.Context(), e.alice, "code"); err != nil {
		t.Errorf("relinking by the owner: %v", err)
	}
}

func TestLinkWithABadCode(t *testing.T) {
	e := newConnectEnv(t)
	e.gh.codeOK = false
	_, err := e.c.Link(t.Context(), e.alice, "expired")
	if err == nil || !strings.Contains(err.Error(), ErrAuthorization.Error()) {
		t.Fatalf("err = %v, want ErrAuthorization", err)
	}
	if !errors.Is(err, ErrAuthorization) {
		t.Errorf("err does not wrap ErrAuthorization: %v", err)
	}
	if strings.Contains(err.Error(), clientSecret) {
		t.Errorf("the error contains the client secret: %v", err)
	}
	if len(e.gh.userCalls) != 0 || len(e.links.rows) != 0 {
		t.Error("something was done without a valid code")
	}

	// An empty code never reaches GitHub.
	e.gh.tokenForm = nil
	if _, err := e.c.Link(t.Context(), e.alice, ""); !errors.Is(err, ErrAuthorization) || e.gh.tokenForm != nil {
		t.Errorf("empty code: %v, form %v", err, e.gh.tokenForm)
	}
}

func TestRepositoriesListsOnlyThroughTheUsersOwnInstallation(t *testing.T) {
	e := newConnectEnv(t)
	e.links.rows[42] = Installation{ID: 42, UserID: e.alice, AccountID: 7, AccountLogin: "octo"}
	e.links.rows[43] = Installation{ID: 43, UserID: uuid.New(), AccountID: 8, AccountLogin: "other"}
	e.gh.totalRepos = 130
	e.gh.repos = []map[string]any{
		{"id": 1, "full_name": "octo/web", "private": true, "default_branch": "main"},
		{"id": 2, "full_name": "octo/docs", "private": false, "default_branch": "trunk"},
	}

	got, err := e.c.Repositories(t.Context(), e.alice, 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 130 || len(got.List) != 2 || got.List[0] != (Repository{ID: 1, FullName: "octo/web", Private: true, DefaultBranch: "main"}) || got.List[1].DefaultBranch != "trunk" {
		t.Errorf("got %+v", got)
	}

	// Someone else's installation, and one that does not exist, look alike, and
	// no token is minted for either.
	before := e.gh.installTokens
	for _, id := range []int64{43, 999} {
		if _, err := e.c.Repositories(t.Context(), e.alice, id); err != ErrNotFound {
			t.Errorf("installation %d: err = %v, want ErrNotFound", id, err)
		}
	}
	if e.gh.installTokens != before {
		t.Error("a token was minted for an installation that is not the user's")
	}
}

func TestAuthorizeAndInstallURLs(t *testing.T) {
	e := newConnectEnv(t)
	u, err := url.Parse(e.c.AuthorizeURL("st ate&x"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "github.com" || u.Path != "/login/oauth/authorize" || q.Get("client_id") != clientID ||
		q.Get("state") != "st ate&x" || q.Get("redirect_uri") != callbackURL || q.Get("response_type") != "code" {
		t.Errorf("authorize URL: %s", u)
	}
	if strings.Contains(u.String(), clientSecret) {
		t.Error("the authorize URL contains the client secret")
	}

	u, err = url.Parse(e.c.InstallURL("abc.123"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "github.com" || u.Path != "/apps/vora-cloud/installations/new" || u.Query().Get("state") != "abc.123" {
		t.Errorf("install URL: %s", u)
	}
}

func TestRepositoryIsFoundThroughTheInstallationsOwnList(t *testing.T) {
	e := newConnectEnv(t)
	e.links.rows[42] = Installation{ID: 42, UserID: e.alice, AccountID: 7, AccountLogin: "octo"}
	e.gh.pageSize = 2
	for i := 1; i <= 5; i++ {
		e.gh.repos = append(e.gh.repos, map[string]any{"id": i, "full_name": fmt.Sprintf("octo/repo%d", i), "private": i%2 == 0, "default_branch": "trunk"})
	}

	// The last one is on the third page.
	got, err := e.c.Repository(t.Context(), e.alice, 42, 5)
	if err != nil {
		t.Fatal(err)
	}
	if *got != (Repository{ID: 5, FullName: "octo/repo5", Private: false, DefaultBranch: "trunk"}) {
		t.Errorf("got %+v", got)
	}
	if e.gh.repoPages != 3 {
		t.Errorf("%d pages fetched, want 3", e.gh.repoPages)
	}
}

// Any token reads any public repository, so asking GitHub for the repository
// by id would accept a stranger's. Only the installation's own list counts.
func TestRepositoryRefusesOnesTheInstallationCannotRead(t *testing.T) {
	e := newConnectEnv(t)
	e.links.rows[42] = Installation{ID: 42, UserID: e.alice, AccountID: 7, AccountLogin: "octo"}
	e.gh.repos = []map[string]any{{"id": 1, "full_name": "octo/web", "default_branch": "main"}}

	if _, err := e.c.Repository(t.Context(), e.alice, 42, 999); err != ErrNotFound {
		t.Errorf("a public repository outside the installation: err = %v, want ErrNotFound", err)
	}
	if _, err := e.c.Repository(t.Context(), e.alice, 42, 12345); err != ErrNotFound {
		t.Errorf("a repository that does not exist: err = %v, want ErrNotFound", err)
	}
}

func TestRepositoryRefusesAnotherUsersInstallation(t *testing.T) {
	e := newConnectEnv(t)
	e.links.rows[43] = Installation{ID: 43, UserID: uuid.New(), AccountID: 8, AccountLogin: "other"}
	e.gh.repos = []map[string]any{{"id": 1, "full_name": "other/secret", "private": true, "default_branch": "main"}}

	before := e.gh.installTokens
	for _, id := range []int64{43, 999} {
		if _, err := e.c.Repository(t.Context(), e.alice, id, 1); err != ErrNotFound {
			t.Errorf("installation %d: err = %v, want ErrNotFound", id, err)
		}
	}
	if e.gh.installTokens != before || e.gh.repoPages != 0 {
		t.Error("GitHub was asked about an installation that is not the user's")
	}
}

// A search that never ends would hold a request open on a huge installation.
func TestRepositorySearchIsBounded(t *testing.T) {
	e := newConnectEnv(t)
	e.links.rows[42] = Installation{ID: 42, UserID: e.alice, AccountID: 7, AccountLogin: "octo"}
	e.gh.pageSize = 1
	for i := 1; i <= maxRepoPages+10; i++ {
		e.gh.repos = append(e.gh.repos, map[string]any{"id": i, "full_name": fmt.Sprintf("octo/r%d", i)})
	}
	if _, err := e.c.Repository(t.Context(), e.alice, 42, int64(maxRepoPages+5)); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound past the bound", err)
	}
	if e.gh.repoPages != maxRepoPages {
		t.Errorf("%d pages fetched, want the bound of %d", e.gh.repoPages, maxRepoPages)
	}
}
