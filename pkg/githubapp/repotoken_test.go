package githubapp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
)

// tokenServer answers the installation token endpoint, recording what it was
// asked for and answering as the test says.
type tokenServer struct {
	mu       sync.Mutex
	requests []map[string]any
	answer   func(n int) string
}

func (s *tokenServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.requests = append(s.requests, body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(s.answer(len(s.requests))))
}

func tokenApp(t *testing.T, srv *tokenServer) *App {
	t.Helper()
	_, keyPEM := testKey(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	base := ts.URL + "/"
	app, err := newApp(clientID, keyPEM, time.Now, github.WithURLs(&base, nil))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func tokenJSON(token string, repoIDs []int, contents string) string {
	var repos []string
	for _, id := range repoIDs {
		repos = append(repos, fmt.Sprintf(`{"id":%d,"full_name":"octo/r%d"}`, id, id))
	}
	return fmt.Sprintf(`{"token":%q,"expires_at":%q,"permissions":{"contents":%q,"metadata":"read"},"repositories":[%s]}`,
		token, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), contents, strings.Join(repos, ","))
}

// The token is asked for one repository and one permission: reading its contents.
func TestRepoTokenAsksForOneRepositoryAndReadOnlyContents(t *testing.T) {
	srv := &tokenServer{answer: func(int) string { return tokenJSON("ghs_one", []int{99}, "read") }}
	app := tokenApp(t, srv)

	got, err := app.RepoToken(t.Context(), 42, 99)
	if err != nil || got != "ghs_one" {
		t.Fatalf("RepoToken = %q, %v", got, err)
	}
	req := srv.requests[0]
	ids, _ := req["repository_ids"].([]any)
	perms, _ := req["permissions"].(map[string]any)
	if len(ids) != 1 || ids[0] != float64(99) {
		t.Errorf("repository_ids = %v", req["repository_ids"])
	}
	if len(perms) != 1 || perms["contents"] != "read" {
		t.Errorf("permissions = %v: a build may read the contents of one repository and nothing more", perms)
	}
	if _, ok := req["repositories"]; ok {
		t.Errorf("repositories (by name) was also sent: %v", req)
	}
}

// Every build gets a token of its own, which goes away with it.
func TestRepoTokenIsNotCached(t *testing.T) {
	srv := &tokenServer{answer: func(n int) string { return tokenJSON(fmt.Sprintf("ghs_%d", n), []int{99}, "read") }}
	app := tokenApp(t, srv)
	a, _ := app.RepoToken(t.Context(), 42, 99)
	b, _ := app.RepoToken(t.Context(), 42, 99)
	if a == b || len(srv.requests) != 2 {
		t.Errorf("tokens %q and %q from %d requests", a, b, len(srv.requests))
	}
}

// If GitHub ignored the scoping, the token would read every repository of the
// installation. It is refused, not used.
func TestRepoTokenRefusesATokenBroaderThanAskedFor(t *testing.T) {
	for name, answer := range map[string]string{
		"two repositories":          tokenJSON("ghs_x", []int{99, 100}, "read"),
		"another repository":        tokenJSON("ghs_x", []int{100}, "read"),
		"no repository (all of it)": tokenJSON("ghs_x", nil, "read"),
		"write access":              tokenJSON("ghs_x", []int{99}, "write"),
		"no contents permission":    tokenJSON("ghs_x", []int{99}, ""),
		"no token":                  tokenJSON("", []int{99}, "read"),
	} {
		t.Run(name, func(t *testing.T) {
			app := tokenApp(t, &tokenServer{answer: func(int) string { return answer }})
			if got, err := app.RepoToken(t.Context(), 42, 99); err == nil || strings.Contains(err.Error(), "ghs_x") {
				t.Errorf("RepoToken = %q, %v; want a refusal that does not echo the token", got, err)
			}
		})
	}
}
