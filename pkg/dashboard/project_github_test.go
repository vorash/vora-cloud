package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/githubapp"
	"github.com/vorash/vora-cloud/pkg/project"
)

// alice's installation 1 can read octo/My.Repo_Name (10); bob's installation 2
// can read bobby/secret (20).
func newRepoEnv(t *testing.T) (*env, *fakeGitHub) {
	t.Helper()
	v, gh := newGitHubEnv(t)
	gh.installs[aliceID] = []githubapp.Installation{{ID: 1, UserID: aliceID, AccountLogin: "octo"}}
	gh.installs[bobID] = []githubapp.Installation{{ID: 2, UserID: bobID, AccountLogin: "bobby"}}
	gh.repos[1] = &githubapp.Repositories{Total: 1, List: []githubapp.Repository{{ID: 10, FullName: "octo/My.Repo_Name", DefaultBranch: "trunk"}}}
	gh.repos[2] = &githubapp.Repositories{Total: 1, List: []githubapp.Repository{{ID: 20, FullName: "bobby/secret", Private: true, DefaultBranch: "main"}}}
	return v, gh
}

func (v *env) postProject(t *testing.T, cookie *http.Cookie, form url.Values) resp {
	t.Helper()
	form.Set("csrf", v.csrf(t, cookie))
	return v.send("POST", "/projects", form, cookie, nil)
}

func (v *env) projectsOf(t *testing.T, user uuid.UUID) []project.Project {
	t.Helper()
	list, err := v.projects.ListProjects(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestGitHubPageLinksEachRepositoryToProjectCreation(t *testing.T) {
	v, _ := newRepoEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	if body := v.send("GET", "/github", nil, c, nil).body; !strings.Contains(body, `href="/projects/new?installation=1&amp;repo=10"`) {
		t.Errorf("no Create project link on the GitHub page: %s", body)
	}
}

func TestNewProjectFromARepositoryIsPrefilledFromGitHub(t *testing.T) {
	v, _ := newRepoEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	r := v.send("GET", "/projects/new?installation=1&repo=10", nil, c, nil)
	if r.code != http.StatusOK {
		t.Fatalf("got %d %s", r.code, r.body)
	}
	for _, want := range []string{
		`value="My.Repo_Name"`, `value="my-repo-name"`, // name and the slug suggested from it
		`https://github.com/octo/My.Repo_Name`, "trunk", // GitHub's own name and branch
		`name="installation" value="1"`, `name="repo" value="10"`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the form lacks %q", want)
		}
	}
	// A plain form offers the way in.
	if body := v.send("GET", "/projects/new", nil, c, nil).body; !strings.Contains(body, `href="/github"`) || strings.Contains(body, `name="repo"`) {
		t.Errorf("the plain form: %s", body)
	}
}

func TestCreatingAProjectFromARepositoryLinksItByGitHubsIds(t *testing.T) {
	v, _ := newRepoEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	// The form also carries values a user could tamper with. None may be used.
	r := v.postProject(t, c, url.Values{
		"name": {"Web"}, "slug": {"web"}, "region": {"eu"}, "installation": {"1"}, "repo": {"10"},
		"full_name": {"evil/other"}, "default_branch": {"hacked"}, "repo_name": {"evil/other"},
	})
	if r.code != http.StatusSeeOther {
		t.Fatalf("got %d %s", r.code, r.body)
	}
	got := v.projectsOf(t, aliceID)
	if len(got) != 1 {
		t.Fatalf("%d projects", len(got))
	}
	p := got[0]
	if p.Repo != "octo/My.Repo_Name" || p.DefaultBranch != "trunk" || p.GitHubInstallationID != 1 || p.GitHubRepoID != 10 {
		t.Errorf("project %+v: the repository must be GitHub's answer for the two ids", p)
	}

	page := v.send("GET", r.header.Get("Location"), nil, c, nil).body
	if !strings.Contains(page, `href="https://github.com/octo/My.Repo_Name"`) {
		t.Errorf("the project page does not show its repository: %s", page)
	}
}

// Only the two ids are believed, and they are checked against the installations
// of the user who sends them.
func TestCreatingFromARepositoryTheUserCannotReadIsRefused(t *testing.T) {
	v, gh := newRepoEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	gh.repos[1].List = append(gh.repos[1].List, githubapp.Repository{ID: 11, FullName: "octo/other"})

	for name, tc := range map[string]struct{ installation, repo string }{
		"another user's installation":                  {"2", "20"},
		"another user's repository in my installation": {"1", "20"},
		"a repository outside the installation":        {"1", "999"},
		"my repository under another installation":     {"2", "10"},
		"an unknown installation":                      {"99", "10"},
		"not numbers":                                  {"x", "y"},
		"zero":                                         {"0", "0"},
		"negative":                                     {"-1", "-10"},
		"only one id":                                  {"1", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if r := v.send("GET", "/projects/new?installation="+tc.installation+"&repo="+tc.repo, nil, c, nil); r.code != http.StatusNotFound {
				t.Errorf("the form: %d, want 404", r.code)
			}
			r := v.postProject(t, c, url.Values{"name": {"x"}, "slug": {"x-" + strings.ReplaceAll(name, " ", "-")[:3]}, "region": {"eu"}, "installation": {tc.installation}, "repo": {tc.repo}})
			if r.code != http.StatusNotFound {
				t.Errorf("the post: %d %s, want 404", r.code, r.body)
			}
		})
	}
	if got := v.projectsOf(t, aliceID); len(got) != 0 {
		t.Errorf("a project was created: %+v", got)
	}
}

// Without the GitHub App the parameters mean nothing, and certainly do not link.
func TestRepositoryParametersAreIgnoredWithoutGitHub(t *testing.T) {
	v := newEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	if r := v.send("GET", "/projects/new?installation=1&repo=10", nil, c, nil); r.code != http.StatusOK || strings.Contains(r.body, `name="repo"`) {
		t.Errorf("the form: %d", r.code)
	}
	r := v.postProject(t, c, url.Values{"name": {"Web"}, "slug": {"web"}, "region": {"eu"}, "installation": {"1"}, "repo": {"10"}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("got %d %s", r.code, r.body)
	}
	if got := v.projectsOf(t, aliceID); len(got) != 1 || got[0].GitHubRepoID != 0 || got[0].GitHubInstallationID != 0 {
		t.Errorf("a link was made without GitHub: %+v", got)
	}
}

func TestGitHubBeingDownDoesNotCreateAProject(t *testing.T) {
	v, gh := newRepoEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	gh.repoErr[1] = errBoom{}

	r := v.send("GET", "/projects/new?installation=1&repo=10", nil, c, nil)
	if r.code != http.StatusBadGateway || !strings.Contains(r.body, "GitHub did not answer") {
		t.Errorf("the form: %d %s", r.code, r.body)
	}
	r = v.postProject(t, c, url.Values{"name": {"Web"}, "slug": {"web"}, "region": {"eu"}, "installation": {"1"}, "repo": {"10"}})
	if r.code != http.StatusBadGateway || !strings.Contains(r.body, `name="repo" value="10"`) || strings.Contains(r.body, "boom") {
		t.Errorf("the post: %d %s", r.code, r.body)
	}
	if got := v.projectsOf(t, aliceID); len(got) != 0 {
		t.Errorf("a project was created while GitHub was down: %+v", got)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom: token ghs_secret" }

func TestTheRepositoryStaysOnTheFormAfterATakenSlug(t *testing.T) {
	v, _ := newRepoEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	v.createProject(t, c, "Existing", "taken")
	r := v.postProject(t, c, url.Values{"name": {"Web"}, "slug": {"taken"}, "region": {"eu"}, "installation": {"1"}, "repo": {"10"}})
	if r.code != http.StatusUnprocessableEntity || !strings.Contains(r.body, `name="repo" value="10"`) || !strings.Contains(r.body, "octo/My.Repo_Name") {
		t.Errorf("got %d: the repository was lost from the form", r.code)
	}
}

// repo is a label the API lets anyone write; it must never look like a
// connection to a repository.
func TestAnAPISetRepoLabelDoesNotLookConnected(t *testing.T) {
	v, _ := newRepoEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	p, err := v.projects.CreateProject(t.Context(), aliceID, project.CreateInput{Name: "Web", Slug: "web", Region: "eu", Repo: "someone/elses-repo"})
	if err != nil {
		t.Fatal(err)
	}
	page := v.send("GET", "/projects/"+p.ID.String(), nil, c, nil).body
	if strings.Contains(page, "someone/elses-repo") || strings.Contains(page, "github.com/") {
		t.Errorf("an unverified label is shown as a repository: %s", page)
	}
}

func TestSlugSuggestions(t *testing.T) {
	for in, want := range map[string]string{
		"My.Repo_Name":                    "my-repo-name",
		"--x--":                           "x",
		"...":                             "",
		"Repo":                            "repo",
		"a  b":                            "a-b",
		"v2.0":                            "v2-0",
		strings.Repeat("ab-", 27) + "end": strings.Repeat("ab-", 20) + "a", // 61 characters, no dash at the end
		strings.Repeat("x", 80):           strings.Repeat("x", 61),
	} {
		if got := slugFor(in); got != want {
			t.Errorf("slugFor(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"My.Repo_Name", strings.Repeat("ab-", 27) + "end", strings.Repeat("x", 80)} {
		if got := slugFor(in); len(got) > 61 || strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Errorf("slugFor(%q) = %q is not a valid slug", in, got)
		}
	}
}
