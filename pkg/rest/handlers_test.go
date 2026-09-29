package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"k8s.io/client-go/kubernetes/fake"

	pb "github.com/vorash/vora-cloud/api/proto"
	"github.com/vorash/vora-cloud/pkg/api"
	"github.com/vorash/vora-cloud/pkg/deploy"
	"github.com/vorash/vora-cloud/pkg/deploy/k3s"
)

func newTestServer(t *testing.T) *echo.Echo {
	t.Helper()
	reg, err := deploy.NewRegistry(map[string]deploy.Deployer{"eu": k3s.New(fake.NewClientset(), k3s.Options{})})
	if err != nil {
		t.Fatal(err)
	}
	svc := api.NewApplicationService(reg)
	resp, _ := svc.DeployApplication(context.Background(), &pb.DeployRequest{
		Name: "web", Image: "registry.local/web:1", Replicas: 1, Cpu: 0.1, Memory: 128, Region: "eu",
	})
	if resp.Status != "SUBMITTED" {
		t.Fatalf("setup deploy: %+v", resp)
	}

	s := &Server{appService: svc}
	e := echo.New()
	e.DELETE("/api/v1/applications/:id", s.deleteApplication)
	e.GET("/api/v1/applications/:id/status", s.getApplicationStatus)
	return e
}

func do(e *echo.Echo, method, path string) (int, string) {
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code, rec.Body.String()
}

// The deployment id is "<region>/<name>". Clients send the slash as %2F, and
// the handler must decode it, or the service sees "eu%2Fweb" and rejects it.
func TestStatusAcceptsAnEscapedDeploymentID(t *testing.T) {
	e := newTestServer(t)
	code, body := do(e, http.MethodGet, "/api/v1/applications/eu%2Fweb/status")
	if code != http.StatusOK || !strings.Contains(body, "pending") {
		t.Errorf("got %d %s, want 200 with the app's status", code, body)
	}
}

func TestDeleteAcceptsAnEscapedDeploymentID(t *testing.T) {
	e := newTestServer(t)
	code, body := do(e, http.MethodDelete, "/api/v1/applications/eu%2Fweb")
	if code != http.StatusOK || !strings.Contains(body, `"success":true`) {
		t.Errorf("got %d %s, want a successful delete", code, body)
	}
}

func TestDeploymentIDParam(t *testing.T) {
	cases := []struct {
		raw, want string
		ok        bool
	}{
		{"eu%2Fweb", "eu/web", true},
		{"eu/web", "eu/web", true},
		{"", "", false},
		{"eu%zzweb", "", false},
	}
	for _, tc := range cases {
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
		c.SetParamNames("id")
		c.SetParamValues(tc.raw)
		got, err := deploymentIDParam(c)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("deploymentIDParam(%q) = %q, %v; want %q, ok=%v", tc.raw, got, err, tc.want, tc.ok)
		}
	}
}
