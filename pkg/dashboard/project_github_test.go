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

// A deployment queued by a push has no image. The page must say what it is
// rather than show an empty cell, and must not offer to redeploy an image that
// does not exist.
func TestAQueuedDeploymentIsShownWithoutAnImage(t *testing.T) {
	v, _ := newRepoEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	p, err := v.projects.CreateProject(t.Context(), aliceID, project.CreateInput{Name: "Web", Slug: "web", Region: "eu", Repo: "octo/web", GitHubInstallationID: 1, GitHubRepoID: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.projects.QueueFromPush(t.Context(), project.PushInput{InstallationID: 1, RepoID: 10, Ref: "refs/heads/main", CommitSHA: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}

	page := v.send("GET", "/projects/"+p.ID.String(), nil, c, nil).body
	for _, want := range []string{"queued", "Queued by a push of <code>aaaaaaa</code>"} {
		if !strings.Contains(page, want) {
			t.Errorf("the project page lacks %q", want)
		}
	}
	if strings.Contains(page, "<code></code>") || strings.Contains(page, "/redeploy") {
		t.Errorf("an empty image, or a Redeploy button with no image to run: %s", page)
	}
	env := v.send("GET", "/projects/"+p.ID.String()+"/env", nil, c, nil).body
	if strings.Contains(env, "/redeploy") || !strings.Contains(env, "Deploy an image from the project page first") {
		t.Errorf("the environment page offers Redeploy with no image")
	}

	// Once there is an image, the older one is what Redeploy runs.
	if _, err := v.projects.Deploy(t.Context(), aliceID, p.ID, project.DeployInput{Image: "reg/web:1"}); err != nil {
		t.Fatal(err)
	}
	page = v.send("GET", "/projects/"+p.ID.String(), nil, c, nil).body
	if !strings.Contains(page, "/redeploy") || !strings.Contains(page, "reg/web:1") {
		t.Errorf("no Redeploy once there is an image")
	}
}

// The Dockerfile's path and the build folder go with the project, as its build
// configuration, and the port is what it listens on.
func TestCreatingFromARepositoryKeepsHowItIsBuilt(t *testing.T) {
	v, _ := newRepoEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	form := v.send("GET", "/projects/new?installation=1&repo=10", nil, c, nil).body
	for _, want := range []string{`name="dockerfile" value="Dockerfile"`, `name="context"`, `name="port" value="80"`} {
		if !strings.Contains(form, want) {
			t.Errorf("the form lacks %q", want)
		}
	}

	r := v.postProject(t, c, url.Values{
		"name": {"Backend"}, "slug": {"backend"}, "region": {"eu"}, "installation": {"1"}, "repo": {"10"},
		"dockerfile": {"Dockerfile.backend"}, "context": {"app"}, "port": {"8000"},
	})
	if r.code != http.StatusSeeOther {
		t.Fatalf("got %d %s", r.code, r.body)
	}
	got := v.projectsOf(t, aliceID)
	if len(got) != 1 || got[0].Port != 8000 {
		t.Fatalf("projects %+v", got)
	}
	settings, err := project.ParseBuildSettings(got[0].BuildConfig)
	if err != nil || settings.Dockerfile != "Dockerfile.backend" || settings.Context != "app" {
		t.Errorf("build settings %+v, %v", settings, err)
	}
}

func TestABadPortOrPathIsRefusedWithTheFormKept(t *testing.T) {
	v, _ := newRepoEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	for name, form := range map[string]url.Values{
		"a port that is not a number": {"port": {"http"}},
		"a port out of range":         {"port": {"70000"}},
		"a dockerfile that leaves":    {"installation": {"1"}, "repo": {"10"}, "dockerfile": {"../../etc/passwd"}},
		"a context that leaves":       {"installation": {"1"}, "repo": {"10"}, "context": {"../x"}},
	} {
		t.Run(name, func(t *testing.T) {
			form.Set("name", "X")
			form.Set("slug", "x-"+strings.ReplaceAll(name, " ", "-")[:4])
			form.Set("region", "eu")
			r := v.postProject(t, c, form)
			if r.code != http.StatusUnprocessableEntity || !strings.Contains(r.body, `name="name" value="X"`) {
				t.Errorf("got %d: the form was lost or the value accepted", r.code)
			}
		})
	}
	if got := v.projectsOf(t, aliceID); len(got) != 0 {
		t.Errorf("a project was created: %+v", got)
	}
}

// A project that is not from a repository has no build to configure: the fields
// are not offered, and if they are sent they are ignored.
func TestAPlainProjectHasNoBuildSettings(t *testing.T) {
	v, _ := newRepoEnv(t)
	c := v.login(t, "alice@example.com", "alice-pass")
	if body := v.send("GET", "/projects/new", nil, c, nil).body; strings.Contains(body, `name="dockerfile"`) || !strings.Contains(body, `name="port"`) {
		t.Error("the plain form offers the wrong fields")
	}
	r := v.postProject(t, c, url.Values{"name": {"P"}, "slug": {"plain"}, "region": {"eu"}, "port": {"3000"}, "dockerfile": {"../evil"}})
	if r.code != http.StatusSeeOther {
		t.Fatalf("got %d %s", r.code, r.body)
	}
	if got := v.projectsOf(t, aliceID); len(got) != 1 || got[0].Port != 3000 || string(got[0].BuildConfig) != "{}" {
		t.Errorf("project %+v", got)
	}
}

func TestTheBuildLogIsServedAsPlainTextToItsOwnerOnly(t *testing.T) {
	v, _ := newRepoEnv(t)
	alice := v.login(t, "alice@example.com", "alice-pass")
	bob := v.login(t, "bob@example.com", "bob-pass")
	p, err := v.projects.CreateProject(t.Context(), aliceID, project.CreateInput{Name: "Web", Slug: "web", Region: "eu", Repo: "octo/web", GitHubInstallationID: 1, GitHubRepoID: 10})
	if err != nil {
		t.Fatal(err)
	}
	q, err := v.projects.QueueFromPush(t.Context(), project.PushInput{InstallationID: 1, RepoID: 10, Ref: "refs/heads/main", CommitSHA: strings.Repeat("a", 40)})
	if err != nil || len(q) != 1 {
		t.Fatal(err)
	}
	dep := q[0].Deployment.ID
	if err := v.projects.RecordBuildLog(t.Context(), dep, "#1 <script>alert(1)</script>\nERROR: boom\n"); err != nil {
		t.Fatal(err)
	}
	path := "/projects/" + p.ID.String() + "/deployments/" + dep.String() + "/log"

	r := v.send("GET", path, nil, alice, nil)
	if r.code != http.StatusOK || !strings.Contains(r.body, "ERROR: boom") {
		t.Fatalf("owner: %d %s", r.code, r.body)
	}
	// Plain text that the browser must not interpret as a page.
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") || r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("Content-Type %q, nosniff %q", ct, r.header.Get("X-Content-Type-Options"))
	}
	if r := v.send("GET", path, nil, bob, nil); r.code != http.StatusNotFound || strings.Contains(r.body, "boom") {
		t.Errorf("another user: %d", r.code)
	}
	if r := v.send("GET", path, nil, nil, nil); r.code != http.StatusSeeOther || r.header.Get("Location") != "/login" {
		t.Errorf("anonymous: %d %s", r.code, r.header.Get("Location"))
	}
	// A deployment of the user's, asked for under another project's path, is not found.
	other, _ := v.projects.CreateProject(t.Context(), aliceID, project.CreateInput{Name: "Other", Slug: "other", Region: "eu"})
	if r := v.send("GET", "/projects/"+other.ID.String()+"/deployments/"+dep.String()+"/log", nil, alice, nil); r.code != http.StatusNotFound {
		t.Errorf("a deployment under another project: %d, want 404", r.code)
	}
	if r := v.send("GET", "/projects/"+p.ID.String()+"/deployments/not-a-uuid/log", nil, alice, nil); r.code != http.StatusNotFound {
		t.Errorf("a malformed id: %d", r.code)
	}
	// The page links to it for a deployment of a commit that has been built.
	if err := v.projects.StartBuild(t.Context(), dep); err != nil {
		t.Fatal(err)
	}
	if page := v.send("GET", "/projects/"+p.ID.String(), nil, alice, nil).body; !strings.Contains(page, `href="`+path+`"`) {
		t.Errorf("the project page has no link to the build log")
	}
}
