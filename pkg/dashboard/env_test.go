package dashboard

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/project"
)

const secretValue = "s3cr3t-value-do-not-leak"

// envEnv is alice with a project and her session; id is the project's path.
func envEnv(t *testing.T) (v *env, alice *http.Cookie, id uuid.UUID) {
	t.Helper()
	v = newEnv(t)
	alice = v.login(t, "alice@example.com", "alice-pass")
	id = uuid.MustParse(strings.TrimPrefix(v.createProject(t, alice, "Web", "web"), "/projects/"))
	return v, alice, id
}

func (v *env) names(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	names, err := v.projects.EnvNames(context.Background(), aliceID, id)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func (v *env) post(t *testing.T, cookie *http.Cookie, path string, form url.Values) resp {
	t.Helper()
	form.Set("csrf", v.csrf(t, cookie))
	return v.send("POST", path, form, cookie, nil)
}

func TestEnvPageNeedsASessionAndAnOwner(t *testing.T) {
	v, alice, id := envEnv(t)
	path := "/projects/" + id.String()
	for _, p := range []string{path + "/env"} {
		if r := v.send("GET", p, nil, nil, nil); r.code != http.StatusSeeOther || r.header.Get("Location") != "/login" {
			t.Errorf("anonymous GET %s: %d", p, r.code)
		}
	}
	for _, p := range []string{path + "/env", path + "/env/delete", path + "/rollback"} {
		if r := v.send("POST", p, url.Values{}, nil, nil); r.code != http.StatusSeeOther || r.header.Get("Location") != "/login" {
			t.Errorf("anonymous POST %s: %d", p, r.code)
		}
		// A signed-in write without the form token is refused, as for every form.
		if r := v.send("POST", p, url.Values{"name": {"A"}}, alice, nil); r.code != http.StatusForbidden {
			t.Errorf("POST %s without a csrf token: %d, want 403", p, r.code)
		}
	}
}

// Alice's variables and deployments are hers: Bob gets 404 for each route and
// changes nothing.
func TestEnvAndRollbackOfAnotherUsersProjectAre404(t *testing.T) {
	v, alice, id := envEnv(t)
	ctx := context.Background()
	if err := v.projects.SetEnv(ctx, aliceID, id, "KEEP", secretValue); err != nil {
		t.Fatal(err)
	}
	dep, _ := v.projects.Deploy(ctx, aliceID, id, project.DeployInput{Image: "reg/web:1"})
	v.settle(t, dep, project.StatusReady, "")
	_ = alice
	bob := v.login(t, "bob@example.com", "bob-pass")
	path := "/projects/" + id.String()

	if r := v.send("GET", path+"/env", nil, bob, nil); r.code != http.StatusNotFound || strings.Contains(r.body, "KEEP") {
		t.Errorf("GET env: %d", r.code)
	}
	for p, form := range map[string]url.Values{
		path + "/env":        {"name": {"EVIL"}, "value": {"x"}},
		path + "/env/delete": {"name": {"KEEP"}},
		path + "/rollback":   {"deployment": {dep.ID.String()}},
	} {
		if r := v.post(t, bob, p, form); r.code != http.StatusNotFound {
			t.Errorf("POST %s: %d, want 404", p, r.code)
		}
	}
	if got := v.names(t, id); len(got) != 1 || got[0] != "KEEP" {
		t.Errorf("alice's variables after bob's requests: %v", got)
	}
	if got, _ := v.projects.ListDeployments(ctx, aliceID, id, project.Page{}); len(got.Deployments) != 1 {
		t.Errorf("bob's rollback deployed to alice's project: %d deployments", len(got.Deployments))
	}
	for _, p := range []string{"/projects/nope/env", "/projects/" + uuid.NewString() + "/env"} {
		if r := v.send("GET", p, nil, bob, nil); r.code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, r.code)
		}
	}
}

// A value is write-only: it must not reach a page, a redirect or the log, on
// the way in or after, including when the form is refused.
func TestEnvValuesNeverComeBack(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	v, alice, id := envEnv(t)
	path := "/projects/" + id.String() + "/env"
	var seen []resp

	r := v.post(t, alice, path, url.Values{"name": {"API_KEY"}, "value": {secretValue}})
	seen = append(seen, r)
	if r.code != http.StatusSeeOther || r.header.Get("Location") != path+"?done=saved" {
		t.Fatalf("save: %d %q", r.code, r.header.Get("Location"))
	}
	page := v.send("GET", path+"?done=saved", nil, alice, nil)
	seen = append(seen, page)
	if !strings.Contains(page.body, "API_KEY") || !strings.Contains(page.body, "applies on the next deployment") {
		t.Errorf("the page lacks the name or the confirmation:\n%s", page.body)
	}
	// A refused form shows the name again, and not the value.
	bad := v.post(t, alice, path, url.Values{"name": {"1BAD NAME"}, "value": {secretValue}})
	seen = append(seen, bad)
	if bad.code != http.StatusUnprocessableEntity || !strings.Contains(bad.body, "1BAD NAME") {
		t.Errorf("a bad name: %d", bad.code)
	}
	seen = append(seen, v.send("GET", "/projects/"+id.String(), nil, alice, nil))

	for i, r := range seen {
		if strings.Contains(r.body, secretValue) || strings.Contains(r.header.Get("Location"), secretValue) {
			t.Errorf("response %d contains the value", i)
		}
	}
	if strings.Contains(logs.String(), secretValue) {
		t.Error("the value reached the log")
	}
	if cc := page.header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("the env page must not be cached: %q", cc)
	}
}

func TestEnvAddReplaceAndRemove(t *testing.T) {
	v, alice, id := envEnv(t)
	path := "/projects/" + id.String() + "/env"
	for _, name := range []string{"ZED", "ALPHA", "ALPHA"} { // the second ALPHA replaces
		if r := v.post(t, alice, path, url.Values{"name": {" " + name + " "}, "value": {"v"}}); r.code != http.StatusSeeOther {
			t.Fatalf("save %s: %d", name, r.code)
		}
	}
	body := v.send("GET", path, nil, alice, nil).body
	if strings.Count(body, "<code>ALPHA</code>") != 1 || strings.Index(body, "<code>ALPHA</code>") > strings.Index(body, "<code>ZED</code>") {
		t.Errorf("names must be listed once and sorted:\n%s", body)
	}
	r := v.post(t, alice, path+"/delete", url.Values{"name": {"ZED"}})
	if r.code != http.StatusSeeOther || r.header.Get("Location") != path+"?done=removed" {
		t.Errorf("remove: %d %q", r.code, r.header.Get("Location"))
	}
	if got := v.names(t, id); len(got) != 1 || got[0] != "ALPHA" {
		t.Errorf("after removing ZED: %v", got)
	}
	// Only the known confirmations are shown; the query string cannot say more.
	if body := v.send("GET", path+"?done=%3Cb%3Ehi", nil, alice, nil).body; strings.Contains(body, "alert-info") || strings.Contains(body, "<b>hi") {
		t.Error("an unknown done value produced a message")
	}
}

func TestProjectPageOffersRollbackOnlyToEarlierReadyDeployments(t *testing.T) {
	v, alice, id := envEnv(t)
	ctx := context.Background()
	deploy := func(image string) *project.Deployment {
		d, err := v.projects.Deploy(ctx, aliceID, id, project.DeployInput{Image: image})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	one := deploy("reg/web:1")
	v.settle(t, one, project.StatusReady, "")
	two := deploy("reg/web:2")
	v.settle(t, two, project.StatusFailed, "rollout timed out")
	three := deploy("reg/web:3") // the newest
	path := "/projects/" + id.String()

	body := v.send("GET", path, nil, alice, nil).body
	if n := strings.Count(body, `action="`+path+`/rollback"`); n != 1 || !strings.Contains(body, `name="deployment" value="`+one.ID.String()+`"`) {
		t.Errorf("want one rollback button, for the ready deployment before the newest; got %d", n)
	}
	// The newest deployment is what runs; rolling back to it would redeploy it.
	v.settle(t, three, project.StatusReady, "")
	body = v.send("GET", path, nil, alice, nil).body
	if n := strings.Count(body, `action="`+path+`/rollback"`); n != 1 || strings.Contains(body, `name="deployment" value="`+three.ID.String()+`"`) {
		t.Errorf("the newest ready deployment must not offer a rollback; got %d buttons", n)
	}
	if !strings.Contains(body, `href="`+path+`/env"`) {
		t.Error("the project page does not link to the environment page")
	}

	r := v.post(t, alice, path+"/rollback", url.Values{"deployment": {one.ID.String()}})
	if r.code != http.StatusSeeOther || !strings.HasPrefix(r.header.Get("Location"), path+"#deployment-") {
		t.Fatalf("rollback: %d %q", r.code, r.header.Get("Location"))
	}
	deps, _ := v.projects.ListDeployments(ctx, aliceID, id, project.Page{})
	if newest := deps.Deployments[0]; newest.Image != "reg/web:1" || newest.ID == one.ID {
		t.Errorf("the rollback did not deploy the earlier image anew: %+v", newest)
	}
	// A deployment that never became ready, or a garbled id, is not a target.
	if r := v.post(t, alice, path+"/rollback", url.Values{"deployment": {two.ID.String()}}); r.code != http.StatusUnprocessableEntity {
		t.Errorf("rollback to a failed deployment: %d, want 422", r.code)
	}
	if r := v.post(t, alice, path+"/rollback", url.Values{"deployment": {"nope"}}); r.code != http.StatusNotFound {
		t.Errorf("rollback to a garbled id: %d, want 404", r.code)
	}
}
