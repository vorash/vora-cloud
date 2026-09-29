//go:build envtest

// These tests run the deployer against a real kube-apiserver and etcd (no
// kubelet, no controllers), so they check what the fake clientset cannot: the
// server's validation, real server-side apply, and defaulting. Run them with
// `make test-envtest`; without the binaries they fail instead of skipping.
package k3s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/vorash/vora-cloud/pkg/deploy"
)

func startAPIServer(t *testing.T) *Deployer {
	t.Helper()
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start kube-apiserver and etcd (run `make test-envtest`): %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return New(client, Options{})
}

func realSpec() deploy.Spec {
	return deploy.Spec{
		Project: "acme", Name: "web", Image: "registry.local/acme/web:abc123",
		Port: 8080, Replicas: 2, CPUMillis: 250, MemoryMiB: 256,
		Host: "web.eu.example.com", TLS: true, HealthPath: "/healthz",
		Env: map[string]string{"DATABASE_URL": "postgres://x", "LOG_LEVEL": "info"},
	}
}

func TestAgainstARealAPIServer(t *testing.T) {
	d := startAPIServer(t)
	ctx := context.Background()
	ns := Namespace("acme")

	deployment := func() (generation int64) {
		dep, err := d.client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get deployment: %v", err)
		}
		return dep.Generation
	}

	// Every object must pass the server's own validation, which the fake skips.
	if err := d.Deploy(ctx, realSpec()); err != nil {
		t.Fatalf("Deploy was rejected by a real API server: %v", err)
	}
	first := deployment()

	// The server turns stringData into data; the app reads the Secret through
	// envFrom, so this is what pods will actually see.
	secret, err := d.client.CoreV1().Secrets(ns).Get(ctx, "web-env", metav1.GetOptions{})
	if err != nil || string(secret.Data["LOG_LEVEL"]) != "info" {
		t.Errorf("secret data = %v, %v; want LOG_LEVEL=info", secret.Data, err)
	}

	dep, _ := d.client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if ru := dep.Spec.Strategy.RollingUpdate; ru == nil || ru.MaxUnavailable.IntValue() != 0 {
		t.Errorf("strategy after the server's defaulting = %+v, want maxUnavailable 0", ru)
	}

	t.Run("deploying the same spec again does not roll the pods", func(t *testing.T) {
		if err := d.Deploy(ctx, realSpec()); err != nil {
			t.Fatal(err)
		}
		if got := deployment(); got != first {
			t.Errorf("generation went from %d to %d on an identical deploy", first, got)
		}
	})

	t.Run("a changed variable rolls the pods", func(t *testing.T) {
		s := realSpec()
		s.Env["LOG_LEVEL"] = "debug"
		if err := d.Deploy(ctx, s); err != nil {
			t.Fatal(err)
		}
		if got := deployment(); got <= first {
			t.Errorf("generation stayed at %d after an environment change, so pods would keep the old value", got)
		}
	})

	t.Run("removing the host removes the ingress", func(t *testing.T) {
		s := realSpec()
		s.Host, s.TLS = "", false
		if err := d.Deploy(ctx, s); err != nil {
			t.Fatal(err)
		}
		l, err := d.client.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{})
		if err != nil || len(l.Items) != 0 {
			t.Errorf("ingresses = %d, %v; want none", len(l.Items), err)
		}
	})

	// With no controllers running, nothing rolls out: the honest status is pending.
	t.Run("status of an app whose pods never start is pending", func(t *testing.T) {
		st, err := d.Status(ctx, "acme", "web")
		if err != nil || st.State != deploy.StatePending {
			t.Errorf("Status = %+v, %v; want pending", st, err)
		}
	})

	t.Run("deleting one app leaves the namespace and its neighbours", func(t *testing.T) {
		other := realSpec()
		other.Name, other.Host, other.TLS = "api", "", false
		if err := d.Deploy(ctx, other); err != nil {
			t.Fatal(err)
		}
		if err := d.Delete(ctx, "acme", "web"); err != nil {
			t.Fatal(err)
		}
		if _, err := d.client.AppsV1().Deployments(ns).Get(ctx, "api", metav1.GetOptions{}); err != nil {
			t.Errorf("deleting web removed api: %v", err)
		}
	})

	// envtest runs no namespace controller, so deletion stays in Terminating.
	t.Run("deleting a project marks its namespace for deletion", func(t *testing.T) {
		if err := d.DeleteProject(ctx, "acme"); err != nil {
			t.Fatal(err)
		}
		got, err := d.client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if err == nil && got.DeletionTimestamp == nil {
			t.Error("namespace is neither gone nor terminating")
		}
	})
}
