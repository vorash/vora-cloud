package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	pb "github.com/vorash/vora-cloud/api/proto"
	"github.com/vorash/vora-cloud/pkg/deploy"
	"github.com/vorash/vora-cloud/pkg/deploy/k3s"
)

type env struct {
	svc      *ApplicationService
	eu, us   *fake.Clientset
	ns, name string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	eu, us := fake.NewClientset(), fake.NewClientset()
	reg, err := deploy.NewRegistry(map[string]deploy.Deployer{
		"eu": k3s.New(eu, k3s.Options{}),
		"us": k3s.New(us, k3s.Options{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &env{svc: NewApplicationService(reg), eu: eu, us: us, ns: k3s.Namespace("web"), name: "web"}
}

func request() *pb.DeployRequest {
	return &pb.DeployRequest{
		Name: "web", Image: "registry.local/web:1", Replicas: 2, Cpu: 0.25, Memory: 256,
		Region: "eu", Labels: map[string]string{"LOG_LEVEL": "info"},
		NetworkMode: pb.NetworkMode_NETWORK_MODE_HOST,
		Traefik:     &pb.TraefikConfig{Enable: true, Host: "web.eu.example.com", EnableSsl: true, HealthCheckPath: "/healthz"},
	}
}

func (e *env) deployments(t *testing.T, c *fake.Clientset) int {
	t.Helper()
	l, err := c.AppsV1().Deployments(e.ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(l.Items)
}

func TestDeployAppliesToTheRequestedRegionOnly(t *testing.T) {
	e := newEnv(t)
	resp, err := e.svc.DeployApplication(context.Background(), request())
	if err != nil || resp.Status != "SUBMITTED" || resp.DeploymentId != "eu/web" {
		t.Fatalf("response = %+v, %v", resp, err)
	}
	if e.deployments(t, e.eu) != 1 {
		t.Error("the eu cluster has no deployment")
	}
	if e.deployments(t, e.us) != 0 {
		t.Error("the us cluster received an app that asked for eu")
	}
}

func TestDeployMapsTheRequestToTheWorkload(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.DeployApplication(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	dep, err := e.eu.AppsV1().Deployments(e.ns).Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if *dep.Spec.Replicas != 2 || c.Image != "registry.local/web:1" {
		t.Errorf("replicas %d image %q", *dep.Spec.Replicas, c.Image)
	}
	if c.Resources.Requests.Cpu().MilliValue() != 250 || c.Resources.Requests.Memory().Value() != 256<<20 {
		t.Errorf("cpu %v memory %v, want 250m and 256Mi", c.Resources.Requests.Cpu(), c.Resources.Requests.Memory())
	}
	if c.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("probe path %q", c.ReadinessProbe.HTTPGet.Path)
	}
	ing, err := e.eu.NetworkingV1().Ingresses(e.ns).Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil || ing.Spec.Rules[0].Host != "web.eu.example.com" {
		t.Errorf("ingress = %+v, %v", ing, err)
	}
}

// An app in a region nobody configured must not land in some other region.
func TestDeployWithAnUnknownOrMissingRegionFailsWithoutFallback(t *testing.T) {
	for _, region := range []string{"", "asia"} {
		e := newEnv(t)
		r := request()
		r.Region = region
		resp, err := e.svc.DeployApplication(context.Background(), r)
		if err != nil || resp.Status != "FAILED" || !strings.Contains(resp.Message, "unknown region") {
			t.Errorf("region %q: response = %+v, %v", region, resp, err)
		}
		if e.deployments(t, e.eu)+e.deployments(t, e.us) != 0 {
			t.Errorf("region %q: something was deployed", region)
		}
	}
}

// These Traefik options were Nomad service tags with no k3s equivalent here.
// Ignoring them would ship an app without the routing the caller asked for.
func TestDeployRejectsTraefikOptionsItCannotHonour(t *testing.T) {
	e := newEnv(t)
	r := request()
	r.Traefik.CertResolver = "letsencrypt"
	r.Traefik.PathPrefix = "/api"
	r.Traefik.Middlewares = []string{"auth"}
	resp, _ := e.svc.DeployApplication(context.Background(), r)
	if resp.Status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", resp.Status)
	}
	for _, opt := range []string{"cert_resolver", "path_prefix", "middlewares"} {
		if !strings.Contains(resp.Message, opt) {
			t.Errorf("message %q does not name %s", resp.Message, opt)
		}
	}
	if e.deployments(t, e.eu) != 0 {
		t.Error("a rejected request still deployed")
	}
}

func TestDeployWithTraefikDisabledCreatesNoRoute(t *testing.T) {
	e := newEnv(t)
	r := request()
	r.Traefik.Enable = false
	if _, err := e.svc.DeployApplication(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	l, _ := e.eu.NetworkingV1().Ingresses(e.ns).List(context.Background(), metav1.ListOptions{})
	if len(l.Items) != 0 {
		t.Errorf("%d ingresses for an app with traefik disabled", len(l.Items))
	}
}

func TestDeployWithoutResourcesFailsValidation(t *testing.T) {
	e := newEnv(t)
	r := request()
	r.Cpu = 0
	resp, _ := e.svc.DeployApplication(context.Background(), r)
	if resp.Status != "FAILED" || !strings.Contains(resp.Message, "cpu and memory") {
		t.Errorf("response = %+v", resp)
	}
}

func TestStatusFollowsTheDeploymentIDToItsRegion(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.DeployApplication(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	st, err := e.svc.GetApplicationStatus(context.Background(), &pb.StatusRequest{DeploymentId: "eu/web"})
	if err != nil || st.JobStatus != "pending" || st.JobType != "service" || st.DesiredInstances != 2 {
		t.Errorf("status = %+v, %v", st, err)
	}
	// The same name in another region is a different, unknown app.
	other, _ := e.svc.GetApplicationStatus(context.Background(), &pb.StatusRequest{DeploymentId: "us/web"})
	if other.JobStatus != "not_found" {
		t.Errorf("us/web status = %q, want not_found", other.JobStatus)
	}
}

// Ids from the Nomad era were bare job names or evaluation ids.
func TestMalformedDeploymentIDsAreRejected(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"", "web", "eu/", "/web", "eu/web/extra", "asia/web"} {
		st, _ := e.svc.GetApplicationStatus(context.Background(), &pb.StatusRequest{DeploymentId: id})
		if !strings.HasPrefix(st.Message, "Failed to get application status") {
			t.Errorf("status of %q = %+v, want a failure", id, st)
		}
		del, _ := e.svc.DeleteApplication(context.Background(), &pb.DeleteRequest{DeploymentId: id})
		if del.Success {
			t.Errorf("delete of %q succeeded", id)
		}
	}
}

func TestDeleteRemovesTheApplicationFromItsRegion(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.DeployApplication(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	resp, err := e.svc.DeleteApplication(context.Background(), &pb.DeleteRequest{DeploymentId: "eu/web"})
	if err != nil || !resp.Success {
		t.Fatalf("delete = %+v, %v", resp, err)
	}
	if _, err := e.eu.CoreV1().Namespaces().Get(context.Background(), e.ns, metav1.GetOptions{}); err == nil {
		t.Error("the namespace survived deleting the application")
	}
}

func TestHealthCheckReportsWhichRegionIsDown(t *testing.T) {
	e := newEnv(t)
	ok, _ := e.svc.HealthCheck(context.Background(), &pb.HealthCheckRequest{})
	if ok.Status != pb.HealthStatus_SERVING {
		t.Fatalf("healthy service reported %v", ok.Status)
	}

	e.us.PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	bad, _ := e.svc.HealthCheck(context.Background(), &pb.HealthCheckRequest{})
	if bad.Status != pb.HealthStatus_NOT_SERVING || !strings.Contains(bad.Message, "us:") || strings.Contains(bad.Message, "eu:") {
		t.Errorf("health = %v %q, want NOT_SERVING naming only us", bad.Status, bad.Message)
	}
}
