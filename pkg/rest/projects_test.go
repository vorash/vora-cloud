package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vorash/vora-cloud/pkg/api"
	"github.com/vorash/vora-cloud/pkg/database"
	"github.com/vorash/vora-cloud/pkg/deploy"
	"github.com/vorash/vora-cloud/pkg/deploy/k3s"
	"github.com/vorash/vora-cloud/pkg/project"
	"github.com/vorash/vora-cloud/pkg/project/projecttest"
	"github.com/vorash/vora-cloud/pkg/secrets"
)

var (
	alice = uuid.NewString()
	bob   = uuid.NewString()
)

// projectsAPI serves the real deploy routes and middleware. Only the login
// step is replaced: the caller is named by a header, and "admin" stands for an
// API key that belongs to no user.
// fakeAccounts records the accounts the handlers create.
type fakeAccounts struct{ created []string }

func (f *fakeAccounts) RegisterUser(_ context.Context, name, email, _ string) (*database.User, error) {
	f.created = append(f.created, email)
	return &database.User{ID: uuid.New(), Name: name, Email: email}, nil
}

func projectsAPI(t *testing.T) *echo.Echo {
	t.Helper()
	reg, err := deploy.NewRegistry(map[string]deploy.Deployer{"eu": k3s.New(fake.NewClientset(), k3s.Options{})})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		accounts:   &fakeAccounts{},
		appService: api.NewApplicationService(reg),
		projects:   project.NewService(projecttest.New(), reg, box, "cloud.example.com"),
	}
	e := echo.New()
	v1 := e.Group("/api/v1", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			who := c.Request().Header.Get("X-Test-User")
			if who == "admin" {
				c.Set(AuthTypeKey, "api_key")
			} else if who != "" {
				c.Set(UserContextKey, who)
				c.Set(AuthTypeKey, "jwt")
			}
			return next(c)
		}
	})
	s.registerDeployRoutes(v1)
	return e
}

func call(t *testing.T, e *echo.Echo, who, method, path, body string) (int, map[string]any) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", who)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil && rec.Body.Len() > 0 {
		t.Fatalf("response is not JSON: %s", rec.Body.String())
	}
	return rec.Code, out
}

func createProject(t *testing.T, e *echo.Echo, who, slug string) string {
	t.Helper()
	code, body := call(t, e, who, "POST", "/api/v1/projects", `{"name":"`+slug+`","slug":"`+slug+`","region":"eu"}`)
	if code != http.StatusCreated {
		t.Fatalf("create project: %d %v", code, body)
	}
	return body["id"].(string)
}

func TestProjectLifecycleThroughTheAPI(t *testing.T) {
	e := projectsAPI(t)
	id := createProject(t, e, alice, "web")

	code, body := call(t, e, alice, "POST", "/api/v1/projects/"+id+"/deployments", `{"image":"reg/web:1","commit_sha":"abc"}`)
	if code != http.StatusCreated || body["status"] != "deploying" || body["url"] != "https://web.eu.cloud.example.com" {
		t.Fatalf("deploy: %d %v", code, body)
	}
	depID := body["id"].(string)

	if code, body := call(t, e, alice, "GET", "/api/v1/deployments/"+depID, ""); code != 200 || body["image"] != "reg/web:1" {
		t.Errorf("get deployment: %d %v", code, body)
	}
	if code, _ := call(t, e, alice, "POST", "/api/v1/projects/"+id+"/redeploy", ""); code != http.StatusCreated {
		t.Errorf("redeploy: %d", code)
	}
	if code, _ := call(t, e, alice, "DELETE", "/api/v1/projects/"+id, ""); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	if code, _ := call(t, e, alice, "GET", "/api/v1/projects/"+id, ""); code != http.StatusNotFound {
		t.Errorf("get after delete: %d", code)
	}
}

// The API face of the ownership rule: another user gets 404, the same answer
// as for an id that does not exist.
func TestAnotherUsersProjectIsNotFound(t *testing.T) {
	e := projectsAPI(t)
	id := createProject(t, e, alice, "web")
	_, dep := call(t, e, alice, "POST", "/api/v1/projects/"+id+"/deployments", `{"image":"reg/web:1"}`)
	depID := dep["id"].(string)

	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/projects/" + id, ""},
		{"DELETE", "/api/v1/projects/" + id, ""},
		{"GET", "/api/v1/projects/" + id + "/deployments", ""},
		{"POST", "/api/v1/projects/" + id + "/deployments", `{"image":"evil:1"}`},
		{"POST", "/api/v1/projects/" + id + "/redeploy", ""},
		{"POST", "/api/v1/projects/" + id + "/rollback", `{"deployment_id":"` + depID + `"}`},
		{"GET", "/api/v1/deployments/" + depID, ""},
		{"GET", "/api/v1/projects/" + id + "/env", ""},
		{"PUT", "/api/v1/projects/" + id + "/env/K", `{"value":"v"}`},
		{"DELETE", "/api/v1/projects/" + id + "/env/K", ""},
		{"GET", "/api/v1/projects/not-a-uuid", ""},
		{"GET", "/api/v1/projects/" + uuid.NewString(), ""},
	} {
		if code, _ := call(t, e, bob, c.method, c.path, c.body); code != http.StatusNotFound {
			t.Errorf("bob %s %s: %d, want 404", c.method, c.path, code)
		}
	}
	if code, _ := call(t, e, alice, "GET", "/api/v1/projects/"+id, ""); code != 200 {
		t.Errorf("the owner lost the project: %d", code)
	}
}

func TestListShowsOnlyTheCallersProjects(t *testing.T) {
	e := projectsAPI(t)
	createProject(t, e, alice, "one")
	createProject(t, e, bob, "two")

	req := httptest.NewRequest("GET", "/api/v1/projects", nil)
	req.Header.Set("X-Test-User", alice)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0]["slug"] != "one" {
		t.Errorf("alice sees %v", list)
	}
}

// Admin keys belong to no user, so they own nothing; and users must not reach
// the ownerless /applications endpoints, which map straight to a namespace.
func TestAdminKeysAndUsersEachGetTheirOwnEndpoints(t *testing.T) {
	e := projectsAPI(t)
	if code, _ := call(t, e, "admin", "POST", "/api/v1/projects", `{"name":"x","slug":"x","region":"eu"}`); code != http.StatusForbidden {
		t.Errorf("admin key on /projects: %d, want 403", code)
	}
	if code, _ := call(t, e, alice, "POST", "/api/v1/applications", `{"name":"x","image":"i"}`); code != http.StatusForbidden {
		t.Errorf("user on /applications: %d, want 403", code)
	}
	if code, _ := call(t, e, alice, "DELETE", "/api/v1/applications/eu%2Fweb", ""); code != http.StatusForbidden {
		t.Errorf("user deleting through /applications: %d, want 403", code)
	}
	// Any user could otherwise mint an ownerless key here and use it on /applications.
	if code, _ := call(t, e, alice, "POST", "/api/v1/admin/keys", `{"name":"mine"}`); code != http.StatusForbidden {
		t.Errorf("user minting an admin key: %d, want 403", code)
	}
	if code, _ := call(t, e, alice, "GET", "/api/v1/admin/keys", ""); code != http.StatusForbidden {
		t.Errorf("user listing admin keys: %d, want 403", code)
	}
	if code, _ := call(t, e, "admin", "POST", "/api/v1/applications", `{"name":"x","image":"i","region":"eu","cpu":0.1,"memory":64,"replicas":1}`); code != http.StatusCreated {
		t.Errorf("admin key on /applications: %d, want 201", code)
	}
	if code, _ := call(t, e, "", "GET", "/api/v1/projects", ""); code != http.StatusForbidden {
		t.Errorf("no identity on /projects: %d, want 403", code)
	}
}

func TestCreateProjectErrors(t *testing.T) {
	e := projectsAPI(t)
	createProject(t, e, alice, "taken")
	for name, c := range map[string]struct {
		body string
		want int
	}{
		"slug of another user": {`{"name":"x","slug":"taken","region":"eu"}`, http.StatusConflict},
		"bad slug":             {`{"name":"x","slug":"Bad_Slug","region":"eu"}`, http.StatusBadRequest},
		"unknown region":       {`{"name":"x","slug":"ok","region":"mars"}`, http.StatusBadRequest},
		"not json":             {`nope`, http.StatusBadRequest},
	} {
		if code, _ := call(t, e, bob, "POST", "/api/v1/projects", c.body); code != c.want {
			t.Errorf("%s: %d, want %d", name, code, c.want)
		}
	}
}

func TestEnvValuesAreNeverReturned(t *testing.T) {
	e := projectsAPI(t)
	id := createProject(t, e, alice, "web")
	if code, _ := call(t, e, alice, "PUT", "/api/v1/projects/"+id+"/env/DATABASE_URL", `{"value":"hunter2"}`); code != http.StatusNoContent {
		t.Fatalf("set: %d", code)
	}
	if code, _ := call(t, e, alice, "PUT", "/api/v1/projects/"+id+"/env/bad-name", `{"value":"x"}`); code != http.StatusBadRequest {
		t.Errorf("invalid name: %d", code)
	}
	req := httptest.NewRequest("GET", "/api/v1/projects/"+id+"/env", nil)
	req.Header.Set("X-Test-User", alice)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "DATABASE_URL") || strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("env listing: %s", rec.Body.String())
	}
}

// Registration is closed by default: anyone could otherwise sign up and deploy
// any image onto the shared node.
func TestRegistrationIsClosedUnlessAllowed(t *testing.T) {
	body := `{"name":"n","email":"a@example.com","password":"longenough"}`
	for _, open := range []bool{false, true} {
		acc := &fakeAccounts{}
		s := &Server{accounts: acc, opts: Options{AllowRegistration: open}}
		e := echo.New()
		e.POST("/register", s.register)
		code, _ := call(t, e, "", "POST", "/register", body)
		want, wantCreated := http.StatusForbidden, 0
		if open {
			want, wantCreated = http.StatusCreated, 1
		}
		if code != want || len(acc.created) != wantCreated {
			t.Errorf("AllowRegistration=%v: %d, %d accounts created; want %d and %d", open, code, len(acc.created), want, wantCreated)
		}
	}
}

func TestOnlyAnAdminKeyCreatesUsers(t *testing.T) {
	e := projectsAPI(t)
	body := `{"name":"n","email":"new@example.com","password":"longenough"}`
	if code, _ := call(t, e, alice, "POST", "/api/v1/admin/users", body); code != http.StatusForbidden {
		t.Errorf("user creating a user: %d, want 403", code)
	}
	if code, out := call(t, e, "admin", "POST", "/api/v1/admin/users", body); code != http.StatusCreated || out["email"] != "new@example.com" {
		t.Errorf("admin creating a user: %d %v", code, out)
	}
	if code, _ := call(t, e, "admin", "POST", "/api/v1/admin/users", `{"name":"n","email":"x@example.com","password":"short"}`); code != http.StatusBadRequest {
		t.Errorf("short password: %d, want 400", code)
	}
}

// The body stays a bare array, so clients written before paging keep working;
// the cursor for older deployments travels in a header.
func TestDeploymentsListPagesThroughTheNextCursorHeader(t *testing.T) {
	e := projectsAPI(t)
	id := createProject(t, e, alice, "web")
	for _, img := range []string{"reg/web:1", "reg/web:2", "reg/web:3"} {
		if code, body := call(t, e, alice, "POST", "/api/v1/projects/"+id+"/deployments", `{"image":"`+img+`"}`); code != http.StatusCreated {
			t.Fatalf("deploy: %d %v", code, body)
		}
	}
	list := func(who, query string) (int, []map[string]any, string) {
		req := httptest.NewRequest("GET", "/api/v1/projects/"+id+"/deployments"+query, nil)
		req.Header.Set("X-Test-User", who)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		var out []map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Header().Get("X-Next-Cursor")
	}

	code, all, next := list(alice, "")
	if code != 200 || len(all) != 3 || next != "" {
		t.Fatalf("no paging asked: %d, %d deployments, next %q; want everything and no cursor", code, len(all), next)
	}
	_, first, next := list(alice, "?limit=2")
	if len(first) != 2 || first[0]["image"] != "reg/web:3" || next == "" {
		t.Fatalf("first page: %v, next %q", first, next)
	}
	_, older, next2 := list(alice, "?limit=2&before="+next)
	if len(older) != 1 || older[0]["image"] != "reg/web:1" || next2 != "" {
		t.Errorf("older page: %v, next %q", older, next2)
	}
	if code, _, _ := list(alice, "?limit=lots"); code != http.StatusBadRequest {
		t.Errorf("a limit that is not a number: %d, want 400", code)
	}
	if code, _, _ := list(alice, "?before=nope"); code != http.StatusBadRequest {
		t.Errorf("a cursor that is not one: %d, want 400", code)
	}
	if code, _, _ := list(bob, "?before="+next); code != http.StatusNotFound {
		t.Errorf("another user with alice's cursor: %d, want 404", code)
	}
}
