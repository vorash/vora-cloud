package k3s

import (
	"context"
	"errors"
	"net/url"
	"syscall"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vorash/vora-cloud/pkg/deploy"
)

func spec() deploy.Spec {
	return deploy.Spec{
		Project: "acme", Name: "web", Image: "registry.local/acme/web:abc123",
		Port: 8080, Replicas: 2, CPUMillis: 250, MemoryMiB: 256,
		Host: "web.eu.example.com", TLS: true, HealthPath: "/healthz",
		Env: map[string]string{"DATABASE_URL": "postgres://x", "LOG_LEVEL": "info"},
	}
}

func newDeployer() (*Deployer, *fake.Clientset) {
	cs := fake.NewClientset()
	return New(cs, Options{}), cs
}

func mustDeploy(t *testing.T, d *Deployer, s deploy.Spec) {
	t.Helper()
	if err := d.Deploy(context.Background(), s); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
}

func getDeployment(t *testing.T, cs *fake.Clientset, project, name string) *appsv1.Deployment {
	t.Helper()
	dep, err := cs.AppsV1().Deployments(Namespace(project)).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment %s/%s: %v", project, name, err)
	}
	return dep
}

func TestDeployBuildsEveryObjectAnExposedAppNeeds(t *testing.T) {
	d, cs := newDeployer()
	mustDeploy(t, d, spec())
	ctx := context.Background()
	ns := Namespace("acme")

	if _, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil {
		t.Fatalf("namespace: %v", err)
	}

	secret, err := cs.CoreV1().Secrets(ns).Get(ctx, "web-env", metav1.GetOptions{})
	if err != nil || secret.StringData["DATABASE_URL"] != "postgres://x" {
		t.Fatalf("env secret = %+v, %v", secret, err)
	}

	dep := getDeployment(t, cs, "acme", "web")
	if *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2", *dep.Spec.Replicas)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != "registry.local/acme/web:abc123" || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("container = image %q port %d", c.Image, c.Ports[0].ContainerPort)
	}
	if got := c.EnvFrom[0].SecretRef.Name; got != "web-env" {
		t.Errorf("env comes from secret %q, want web-env", got)
	}

	svc, err := cs.CoreV1().Services(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	p := svc.Spec.Ports[0]
	if p.Port != 80 || p.TargetPort != intstr.FromInt32(8080) {
		t.Errorf("service port %d -> %v, want 80 -> 8080", p.Port, p.TargetPort)
	}
	if svc.Spec.Selector["vora.sh/app"] != "web" {
		t.Errorf("service selector = %v", svc.Spec.Selector)
	}

	ing, err := cs.NetworkingV1().Ingresses(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ingress: %v", err)
	}
	rule := ing.Spec.Rules[0]
	backend := rule.HTTP.Paths[0].Backend.Service
	if *ing.Spec.IngressClassName != "traefik" || rule.Host != "web.eu.example.com" ||
		backend.Name != "web" || backend.Port.Number != 80 {
		t.Errorf("ingress routes %q via %q to %s:%d", rule.Host, *ing.Spec.IngressClassName, backend.Name, backend.Port.Number)
	}
}

// Traffic must never be sent to a pod that is not serving, and a rollout must
// never take a serving pod away before its replacement is ready.
func TestDeploymentIsSafeToRollOut(t *testing.T) {
	d, cs := newDeployer()
	mustDeploy(t, d, spec())
	dep := getDeployment(t, cs, "acme", "web")

	ru := dep.Spec.Strategy.RollingUpdate
	if ru == nil || ru.MaxUnavailable.IntValue() != 0 || ru.MaxSurge.IntValue() != 1 {
		t.Errorf("rolling update = %+v, want maxUnavailable 0 and maxSurge 1", ru)
	}
	probe := dep.Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Path != "/healthz" || probe.HTTPGet.Port != intstr.FromInt32(8080) {
		t.Errorf("readiness probe = %+v, want HTTP GET /healthz on 8080", probe)
	}
	if dep.Spec.ProgressDeadlineSeconds == nil || *dep.Spec.ProgressDeadlineSeconds != progressDeadlineSeconds {
		t.Errorf("progress deadline = %v, want %d", dep.Spec.ProgressDeadlineSeconds, progressDeadlineSeconds)
	}
}

func TestAppWithoutHealthPathIsCheckedByTCP(t *testing.T) {
	d, cs := newDeployer()
	s := spec()
	s.HealthPath = ""
	mustDeploy(t, d, s)
	probe := getDeployment(t, cs, "acme", "web").Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.TCPSocket == nil || probe.HTTPGet != nil {
		t.Errorf("probe = %+v, want a TCP check and no HTTP check", probe)
	}
}

// Requests equal limits, so an app gets exactly what it asked for and cannot
// take memory from its neighbours.
func TestResourcesAreRequestAndLimit(t *testing.T) {
	d, cs := newDeployer()
	mustDeploy(t, d, spec())
	r := getDeployment(t, cs, "acme", "web").Spec.Template.Spec.Containers[0].Resources
	if r.Requests.Cpu().MilliValue() != 250 || r.Limits.Cpu().MilliValue() != 250 {
		t.Errorf("cpu request %v limit %v, want 250m both", r.Requests.Cpu(), r.Limits.Cpu())
	}
	const want = 256 << 20
	if r.Requests.Memory().Value() != want || r.Limits.Memory().Value() != want {
		t.Errorf("memory request %v limit %v, want 256Mi both", r.Requests.Memory(), r.Limits.Memory())
	}
}

func TestPodsGetNoKubernetesAPIToken(t *testing.T) {
	d, cs := newDeployer()
	mustDeploy(t, d, spec())
	auto := getDeployment(t, cs, "acme", "web").Spec.Template.Spec.AutomountServiceAccountToken
	if auto == nil || *auto {
		t.Errorf("automountServiceAccountToken = %v, want false", auto)
	}
}

func TestPlainHTTPAppUsesTheWebEntrypointWithoutTLS(t *testing.T) {
	d, cs := newDeployer()
	s := spec()
	s.TLS = false
	mustDeploy(t, d, s)
	ing, err := cs.NetworkingV1().Ingresses(Namespace("acme")).Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a := ing.Annotations
	if a["traefik.ingress.kubernetes.io/router.entrypoints"] != "web" || a["traefik.ingress.kubernetes.io/router.tls"] != "" {
		t.Errorf("annotations = %v, want web entrypoint and no tls", a)
	}
}

func TestTLSAppUsesTheSecureEntrypoint(t *testing.T) {
	d, cs := newDeployer()
	mustDeploy(t, d, spec())
	ing, err := cs.NetworkingV1().Ingresses(Namespace("acme")).Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a := ing.Annotations
	if a["traefik.ingress.kubernetes.io/router.entrypoints"] != "websecure" || a["traefik.ingress.kubernetes.io/router.tls"] != "true" {
		t.Errorf("annotations = %v, want websecure entrypoint and tls", a)
	}
}

func TestDeployTwiceChangesNothing(t *testing.T) {
	d, cs := newDeployer()
	mustDeploy(t, d, spec())
	mustDeploy(t, d, spec())
	deps, _ := cs.AppsV1().Deployments(Namespace("acme")).List(context.Background(), metav1.ListOptions{})
	svcs, _ := cs.CoreV1().Services(Namespace("acme")).List(context.Background(), metav1.ListOptions{})
	if len(deps.Items) != 1 || len(svcs.Items) != 1 {
		t.Errorf("got %d deployments and %d services after two identical deploys, want 1 and 1", len(deps.Items), len(svcs.Items))
	}
}

func TestRedeployAppliesNewImageAndReplicas(t *testing.T) {
	d, cs := newDeployer()
	mustDeploy(t, d, spec())
	s := spec()
	s.Image, s.Replicas = "registry.local/acme/web:def456", 4
	mustDeploy(t, d, s)
	dep := getDeployment(t, cs, "acme", "web")
	if *dep.Spec.Replicas != 4 || dep.Spec.Template.Spec.Containers[0].Image != "registry.local/acme/web:def456" {
		t.Errorf("after redeploy: replicas %d image %q", *dep.Spec.Replicas, dep.Spec.Template.Spec.Containers[0].Image)
	}
}

// A changed variable lands in the Secret, but running pods keep the old value
// until they restart; the hash on the pod template is what makes them roll.
func TestChangedEnvironmentRollsThePods(t *testing.T) {
	d, cs := newDeployer()
	mustDeploy(t, d, spec())
	before := getDeployment(t, cs, "acme", "web").Spec.Template.Annotations[envHashAnnot]

	same := spec()
	mustDeploy(t, d, same)
	if got := getDeployment(t, cs, "acme", "web").Spec.Template.Annotations[envHashAnnot]; got != before {
		t.Errorf("hash changed from %q to %q with the same environment", before, got)
	}

	changed := spec()
	changed.Env["LOG_LEVEL"] = "debug"
	mustDeploy(t, d, changed)
	if got := getDeployment(t, cs, "acme", "web").Spec.Template.Annotations[envHashAnnot]; got == before {
		t.Error("hash did not change when a variable changed, so pods would keep the old value")
	}
}

func TestEnvHashCannotBeCollidedByShiftingCharactersBetweenKeyAndValue(t *testing.T) {
	a := envHash(map[string]string{"AB": "C"})
	b := envHash(map[string]string{"A": "BC"})
	if a == b {
		t.Fatal("different environments produced the same hash")
	}
}

func TestRemovingTheHostStopsRoutingToTheApp(t *testing.T) {
	d, cs := newDeployer()
	mustDeploy(t, d, spec())
	s := spec()
	s.Host, s.TLS = "", false
	mustDeploy(t, d, s)
	_, err := cs.NetworkingV1().Ingresses(Namespace("acme")).Get(context.Background(), "web", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("ingress still exists after the host was removed: %v", err)
	}
}

func TestDeployWithoutAHostCreatesNoIngress(t *testing.T) {
	d, cs := newDeployer()
	s := spec()
	s.Host, s.TLS = "", false
	mustDeploy(t, d, s)
	ings, _ := cs.NetworkingV1().Ingresses(Namespace("acme")).List(context.Background(), metav1.ListOptions{})
	if len(ings.Items) != 0 {
		t.Errorf("%d ingresses for an unexposed app", len(ings.Items))
	}
}

func TestInvalidSpecTouchesNoCluster(t *testing.T) {
	d, cs := newDeployer()
	s := spec()
	s.Name = "Not_Valid"
	err := d.Deploy(context.Background(), s)
	if !errors.Is(err, deploy.ErrInvalidSpec) {
		t.Fatalf("got %v, want ErrInvalidSpec", err)
	}
	if n := len(cs.Actions()); n != 0 {
		t.Errorf("%d cluster calls for an invalid spec, want 0", n)
	}
}

// If the workload cannot be applied, nothing may route traffic to it.
func TestFailedDeploymentApplyLeavesNoRoute(t *testing.T) {
	d, cs := newDeployer()
	cs.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	if err := d.Deploy(context.Background(), spec()); err == nil {
		t.Fatal("expected the apply failure to be returned")
	}
	ings, _ := cs.NetworkingV1().Ingresses(Namespace("acme")).List(context.Background(), metav1.ListOptions{})
	svcs, _ := cs.CoreV1().Services(Namespace("acme")).List(context.Background(), metav1.ListOptions{})
	if len(ings.Items) != 0 || len(svcs.Items) != 0 {
		t.Errorf("%d ingresses and %d services exist for an app whose deployment failed", len(ings.Items), len(svcs.Items))
	}
}

func TestProjectsAreIsolatedInSeparateNamespaces(t *testing.T) {
	d, cs := newDeployer()
	a, b := spec(), spec()
	b.Project = "globex"
	b.Image = "registry.local/globex/web:1"
	mustDeploy(t, d, a)
	mustDeploy(t, d, b)
	if got := getDeployment(t, cs, "acme", "web").Spec.Template.Spec.Containers[0].Image; got != a.Image {
		t.Errorf("acme runs %q", got)
	}
	if got := getDeployment(t, cs, "globex", "web").Spec.Template.Spec.Containers[0].Image; got != b.Image {
		t.Errorf("globex runs %q", got)
	}
}

func TestDeleteRemovesOneAppAndKeepsItsNeighbour(t *testing.T) {
	d, cs := newDeployer()
	mustDeploy(t, d, spec())
	other := spec()
	other.Name, other.Host = "api", "api.eu.example.com"
	mustDeploy(t, d, other)

	if err := d.Delete(context.Background(), "acme", "web"); err != nil {
		t.Fatal(err)
	}
	ctx, ns := context.Background(), Namespace("acme")
	if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("web deployment survived: %v", err)
	}
	if _, err := cs.CoreV1().Secrets(ns).Get(ctx, "web-env", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("web env secret survived: %v", err)
	}
	if _, err := cs.NetworkingV1().Ingresses(ns).Get(ctx, "web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("web ingress survived: %v", err)
	}
	if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "api", metav1.GetOptions{}); err != nil {
		t.Errorf("deleting web removed api: %v", err)
	}
}

func TestDeleteOfAnAppThatIsGoneSucceeds(t *testing.T) {
	d, _ := newDeployer()
	if err := d.Delete(context.Background(), "acme", "web"); err != nil {
		t.Errorf("deleting a missing app failed: %v", err)
	}
	if err := d.DeleteProject(context.Background(), "acme"); err != nil {
		t.Errorf("deleting a missing project failed: %v", err)
	}
}

func TestDeleteProjectRemovesItsNamespace(t *testing.T) {
	d, cs := newDeployer()
	mustDeploy(t, d, spec())
	if err := d.DeleteProject(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), Namespace("acme"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("namespace survived project deletion: %v", err)
	}
}

func i32(v int32) *int32 { return &v }

func rollout(generation, observed int64, desired, replicas, updated, ready, available int32, conds ...appsv1.DeploymentCondition) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: Namespace("acme"), Generation: generation},
		Spec:       appsv1.DeploymentSpec{Replicas: i32(desired)},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: observed, Replicas: replicas, UpdatedReplicas: updated,
			ReadyReplicas: ready, AvailableReplicas: available, Conditions: conds,
		},
	}
}

func TestStatusReadsTheRolloutTheWayKubectlDoes(t *testing.T) {
	deadline := appsv1.DeploymentCondition{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
		Reason: "ProgressDeadlineExceeded", Message: "ReplicaSet web-abc has timed out progressing.",
	}
	cases := []struct {
		name string
		dep  *appsv1.Deployment
		want deploy.State
	}{
		{"controller has not seen the latest spec", rollout(2, 1, 2, 2, 2, 2, 2), deploy.StatePending},
		{"no replica is on the new spec yet", rollout(1, 1, 2, 0, 0, 0, 0), deploy.StatePending},
		{"new pods up, old pods still serving", rollout(2, 2, 2, 3, 2, 2, 2), deploy.StateRollingOut},
		{"all updated but one not ready", rollout(1, 1, 2, 2, 2, 1, 1), deploy.StateRollingOut},
		{"everything updated, ready and available", rollout(1, 1, 2, 2, 2, 2, 2), deploy.StateReady},
		{"paused at zero replicas", rollout(3, 3, 0, 0, 0, 0, 0), deploy.StateReady},
		{"no progress within the deadline", rollout(2, 2, 2, 3, 1, 1, 1, deadline), deploy.StateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusOf(tc.dep).State; got != tc.want {
				t.Errorf("state = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestStatusCarriesTheFailureReason(t *testing.T) {
	dep := rollout(2, 2, 2, 3, 1, 1, 1, appsv1.DeploymentCondition{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
		Reason: "ProgressDeadlineExceeded", Message: "timed out progressing",
	})
	got := statusOf(dep)
	if got.Message != "timed out progressing" || got.Reason != "ProgressDeadlineExceeded" {
		t.Errorf("reason %q, message %q", got.Reason, got.Message)
	}
}

// The service tells an unreachable cluster from a refused spec by these
// markers, not by reading the cluster's text.
func TestClusterErrorsAreMarkedForTheCaller(t *testing.T) {
	gr := schema.GroupResource{Resource: "deployments"}
	refused := func(err error) bool { return errors.Is(err, deploy.ErrRefused) }
	unreachable := func(err error) bool { return errors.Is(err, deploy.ErrUnreachable) }
	cases := []struct {
		name string
		err  error
		is   func(error) bool
	}{
		{"connection refused", &url.Error{Op: "Patch", URL: "https://127.0.0.1:1/api", Err: syscall.ECONNREFUSED}, unreachable},
		{"deadline", context.DeadlineExceeded, unreachable},
		{"service unavailable", apierrors.NewServiceUnavailable("etcd"), unreachable},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "Deployment"}, "web", nil), refused},
		{"forbidden", apierrors.NewForbidden(gr, "web", errors.New("quota")), refused},
		{"bad request", apierrors.NewBadRequest("no"), refused},
	}
	for _, c := range cases {
		err := clusterErr("apply deployment", c.err)
		if !c.is(err) || !errors.Is(err, c.err) {
			t.Errorf("%s: %v is not marked, or lost its cause", c.name, err)
		}
	}
	other := clusterErr("apply deployment", apierrors.NewInternalError(errors.New("x")))
	if errors.Is(other, deploy.ErrRefused) || errors.Is(other, deploy.ErrUnreachable) {
		t.Errorf("an unclassified error must stay unmarked: %v", other)
	}
}

func TestStatusOfAnUnknownAppIsNotFoundNotAnError(t *testing.T) {
	d, _ := newDeployer()
	st, err := d.Status(context.Background(), "acme", "missing")
	if err != nil || st.State != deploy.StateNotFound {
		t.Errorf("Status = %+v, %v; want not_found and no error", st, err)
	}
}

func TestStatusReadsTheClusterObject(t *testing.T) {
	cs := fake.NewClientset(rollout(1, 1, 2, 2, 2, 2, 2))
	st, err := New(cs, Options{}).Status(context.Background(), "acme", "web")
	if err != nil || st.State != deploy.StateReady || st.Desired != 2 || st.Ready != 2 {
		t.Errorf("Status = %+v, %v", st, err)
	}
}

func TestHealthIsNilWhenTheAPIServerAnswers(t *testing.T) {
	d, _ := newDeployer()
	if err := d.Health(context.Background()); err != nil {
		t.Errorf("Health = %v, want nil", err)
	}
}

func TestHealthReportsAnUnreachableAPIServer(t *testing.T) {
	d, cs := newDeployer()
	cs.PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	if err := d.Health(context.Background()); err == nil {
		t.Error("Health = nil, want an error when the API server is down")
	}
}

// clientcmd falls back to in-cluster or default config for an empty path,
// which would quietly point a region at the wrong cluster.
func TestNewFromKubeconfigRejectsAMissingPathInsteadOfFallingBack(t *testing.T) {
	if _, err := NewFromKubeconfig("", Options{}); err == nil {
		t.Error("expected an error for an empty kubeconfig path")
	}
	if _, err := NewFromKubeconfig("/nonexistent/kubeconfig", Options{}); err == nil {
		t.Error("expected an error for an unreadable kubeconfig")
	}
}
