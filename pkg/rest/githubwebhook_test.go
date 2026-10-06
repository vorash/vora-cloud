package rest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vorash/vora-cloud/pkg/auth"
	"github.com/vorash/vora-cloud/pkg/dashboard"
	"github.com/vorash/vora-cloud/pkg/deploy"
	"github.com/vorash/vora-cloud/pkg/deploy/k3s"
	"github.com/vorash/vora-cloud/pkg/githubapp"
	"github.com/vorash/vora-cloud/pkg/project"
	"github.com/vorash/vora-cloud/pkg/project/projecttest"
	"github.com/vorash/vora-cloud/pkg/secrets"
)

type seenDeliveries map[string]bool

func (s seenDeliveries) Record(_ context.Context, id, _ string) (bool, error) {
	fresh := !s[id]
	s[id] = true
	return fresh, nil
}

func (s seenDeliveries) Forget(_ context.Context, id string) error {
	delete(s, id)
	return nil
}

// GitHub sends no session, no API key and no Origin. The real route table has
// authentication on /api/v1 and an origin check on the dashboard's writes, so
// the webhook has to get past both on the strength of its signature alone.
func TestGitHubWebhookIsReachableThroughTheRealRoutes(t *testing.T) {
	s := &Server{echo: echo.New()}
	s.setupRoutes()
	dash, err := dashboard.New(dashboard.Config{
		CSRFKey: bytes.Repeat([]byte{1}, 32), Limiter: auth.NewAttemptLimiter(time.Second, 1), PublicURL: "https://cloud.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	dash.Register(s.echo)
	hook, err := githubapp.NewWebhook("s3cret", seenDeliveries{})
	if err != nil {
		t.Fatal(err)
	}
	hook.Register(s.echo)

	body := []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	post := func(signature string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, githubapp.WebhookPath, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-GitHub-Delivery", "d-1")
		req.Header.Set("X-Hub-Signature-256", signature)
		rec := httptest.NewRecorder()
		s.echo.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("sha256=" + hex.EncodeToString(mac.Sum(nil))); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "accepted") {
		t.Errorf("signed delivery got %d %s", rec.Code, rec.Body)
	}
	// 401 from the handler, not from the API's own authentication.
	if rec := post("sha256=" + strings.Repeat("00", 32)); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid signature") {
		t.Errorf("wrongly signed delivery got %d %s", rec.Code, rec.Body)
	}
}

// A signed push, through the real routes and the real project service, queues a
// deployment for the project tied to the repository by GitHub's ids, and for no
// other: not for a project that only carries the repository's name.
func TestAPushQueuesADeploymentThroughTheRealRoutes(t *testing.T) {
	reg, err := deploy.NewRegistry(map[string]deploy.Deployer{"eu": k3s.New(fake.NewClientset(), k3s.Options{})})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := projecttest.New()
	svc := project.NewService(store, reg, box, "cloud.example.com")
	owner, other := uuid.New(), uuid.New()
	linked, err := svc.CreateProject(context.Background(), owner, project.CreateInput{
		Name: "Invoicing", Slug: "invoicing", Region: "eu", Repo: "octocat/invoicing-platform", GitHubInstallationID: 168273140, GitHubRepoID: 1350744260,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := svc.CreateProject(context.Background(), other, project.CreateInput{Name: "Claim", Slug: "claim", Region: "eu", Repo: "octocat/invoicing-platform"})
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{echo: echo.New()}
	s.setupRoutes()
	hook, err := githubapp.NewWebhook("s3cret", seenDeliveries{})
	if err != nil {
		t.Fatal(err)
	}
	pushes := githubapp.NewPushDeployments(svc, nil, "https://cloud.test")
	hook.WithPushHandler(pushes).Register(s.echo)

	body := []byte(`{"ref":"refs/heads/main","after":"d8f1b0a4c91e3f0e4a2b7a9c5d6e8f1a2b3c4d5e","deleted":false,
		"repository":{"id":1350744260,"full_name":"octocat/invoicing-platform","default_branch":"main"},"installation":{"id":168273140}}`)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, githubapp.WebhookPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "d-push")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	s.echo.ServeHTTP(rec, req)
	pushes.Wait()

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	deps, err := store.ListDeployments(context.Background(), linked.ID, nil, 10)
	if err != nil || len(deps) != 1 || deps[0].Status != project.StatusQueued || deps[0].CommitSHA != "d8f1b0a4c91e3f0e4a2b7a9c5d6e8f1a2b3c4d5e" {
		t.Fatalf("the linked project's deployments: %+v, %v", deps, err)
	}
	if deps, _ := store.ListDeployments(context.Background(), claim.ID, nil, 10); len(deps) != 0 {
		t.Errorf("a project that only carries the repository's name got %+v", deps)
	}
}
