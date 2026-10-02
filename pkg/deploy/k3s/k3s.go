// Package k3s deploys apps to a Kubernetes cluster (k3s in practice) with
// server-side apply, so Deploy can be repeated safely.
package k3s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"net"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	netv1ac "k8s.io/client-go/applyconfigurations/networking/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/vorash/vora-cloud/pkg/deploy"
)

const (
	fieldManager  = "vora-cloud"
	appLabel      = "vora.sh/app"
	projectLabel  = "vora.sh/project"
	envHashAnnot  = "vora.sh/env-hash"
	managedByKey  = "app.kubernetes.io/managed-by"
	containerName = "app"
	// servicePort is what the Service exposes; the container port is Spec.Port.
	servicePort = 80
	// progressDeadlineSeconds is how long a rollout may make no progress before
	// Status reports it as failed.
	progressDeadlineSeconds = 300
)

// Options tunes a Deployer.
type Options struct {
	// IngressClass is the class Ingresses use. k3s ships Traefik as "traefik".
	IngressClass string
}

// Deployer implements deploy.Deployer against one cluster.
type Deployer struct {
	client       kubernetes.Interface
	ingressClass string
}

var _ deploy.Deployer = (*Deployer)(nil)

func New(client kubernetes.Interface, opts Options) *Deployer {
	if opts.IngressClass == "" {
		opts.IngressClass = "traefik"
	}
	return &Deployer{client: client, ingressClass: opts.IngressClass}
}

// NewFromKubeconfig connects to the cluster a kubeconfig file points at. An
// empty path is an error: clientcmd would otherwise fall back to whatever
// cluster the environment happens to describe.
func NewFromKubeconfig(path string, opts Options) (*Deployer, error) {
	if path == "" {
		return nil, fmt.Errorf("k3s: kubeconfig path is required")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("k3s: load kubeconfig %s: %w", path, err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k3s: create client: %w", err)
	}
	return New(client, opts), nil
}

// Health asks the API server for its version, which needs no permissions
// beyond reaching it.
func (d *Deployer) Health(context.Context) error {
	if _, err := d.client.Discovery().ServerVersion(); err != nil {
		return fmt.Errorf("k3s: api server unreachable: %w", err)
	}
	return nil
}

// Namespace is the namespace a project's apps live in.
func Namespace(project string) string { return "p-" + project }

func (d *Deployer) Deploy(ctx context.Context, spec deploy.Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	ns := Namespace(spec.Project)
	opts := metav1.ApplyOptions{FieldManager: fieldManager, Force: true}

	if _, err := d.client.CoreV1().Namespaces().Apply(ctx, namespace(spec), opts); err != nil {
		return clusterErr(fmt.Sprintf("apply namespace %s", ns), err)
	}
	if _, err := d.client.CoreV1().Secrets(ns).Apply(ctx, envSecret(spec), opts); err != nil {
		return clusterErr("apply env secret", err)
	}
	if _, err := d.client.AppsV1().Deployments(ns).Apply(ctx, deployment(spec), opts); err != nil {
		return clusterErr("apply deployment", err)
	}
	if _, err := d.client.CoreV1().Services(ns).Apply(ctx, service(spec), opts); err != nil {
		return clusterErr("apply service", err)
	}

	// An app whose host was removed must stop being routed.
	if spec.Host == "" {
		err := d.client.NetworkingV1().Ingresses(ns).Delete(ctx, spec.Name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return clusterErr("delete ingress", err)
		}
		return nil
	}
	if _, err := d.client.NetworkingV1().Ingresses(ns).Apply(ctx, ingress(spec, d.ingressClass), opts); err != nil {
		return clusterErr("apply ingress", err)
	}
	return nil
}

// clusterErr wraps a failed cluster call so a caller can tell an unreachable
// cluster from a refused spec with errors.Is, without reading the text. The
// text stays as it was, for the operator's log.
func clusterErr(what string, err error) error {
	var netErr net.Error
	switch {
	case errors.IsInvalid(err) || errors.IsBadRequest(err) || errors.IsForbidden(err):
		return fmt.Errorf("%s: %w: %w", what, deploy.ErrRefused, err)
	case stderrors.As(err, &netErr) || stderrors.Is(err, context.DeadlineExceeded) ||
		errors.IsServiceUnavailable(err) || errors.IsServerTimeout(err) || errors.IsTimeout(err):
		return fmt.Errorf("%s: %w: %w", what, deploy.ErrUnreachable, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func (d *Deployer) Status(ctx context.Context, project, name string) (deploy.Status, error) {
	dep, err := d.client.AppsV1().Deployments(Namespace(project)).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return deploy.Status{State: deploy.StateNotFound}, nil
	}
	if err != nil {
		return deploy.Status{}, fmt.Errorf("get deployment: %w", err)
	}
	return statusOf(dep), nil
}

func (d *Deployer) Delete(ctx context.Context, project, name string) error {
	ns := Namespace(project)
	// Stop routing first, then the workload, then what it depended on.
	steps := []struct {
		what string
		do   func() error
	}{
		{"ingress", func() error {
			return d.client.NetworkingV1().Ingresses(ns).Delete(ctx, name, metav1.DeleteOptions{})
		}},
		{"service", func() error {
			return d.client.CoreV1().Services(ns).Delete(ctx, name, metav1.DeleteOptions{})
		}},
		{"deployment", func() error {
			return d.client.AppsV1().Deployments(ns).Delete(ctx, name, metav1.DeleteOptions{})
		}},
		{"env secret", func() error {
			return d.client.CoreV1().Secrets(ns).Delete(ctx, envSecretName(name), metav1.DeleteOptions{})
		}},
	}
	for _, s := range steps {
		if err := s.do(); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete %s: %w", s.what, err)
		}
	}
	return nil
}

func (d *Deployer) DeleteProject(ctx context.Context, project string) error {
	err := d.client.CoreV1().Namespaces().Delete(ctx, Namespace(project), metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete namespace: %w", err)
	}
	return nil
}

func labels(spec deploy.Spec) map[string]string {
	return map[string]string{
		managedByKey: fieldManager,
		projectLabel: spec.Project,
		appLabel:     spec.Name,
	}
}

func selector(spec deploy.Spec) map[string]string {
	return map[string]string{appLabel: spec.Name}
}

func envSecretName(app string) string { return app + "-env" }

func namespace(spec deploy.Spec) *corev1ac.NamespaceApplyConfiguration {
	return corev1ac.Namespace(Namespace(spec.Project)).WithLabels(map[string]string{
		managedByKey: fieldManager,
		projectLabel: spec.Project,
	})
}

func envSecret(spec deploy.Spec) *corev1ac.SecretApplyConfiguration {
	return corev1ac.Secret(envSecretName(spec.Name), Namespace(spec.Project)).
		WithLabels(labels(spec)).
		WithType(corev1.SecretTypeOpaque).
		WithStringData(spec.Env)
}

// envHash changes whenever the environment does, so a changed variable rolls
// the pods: a Secret update alone would leave running pods on the old value.
func envHash(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "%d:%s=%d:%s\n", len(k), k, len(env[k]), env[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func deployment(spec deploy.Spec) *appsv1ac.DeploymentApplyConfiguration {
	resources := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(spec.CPUMillis, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(spec.MemoryMiB<<20, resource.BinarySI),
	}

	probe := corev1ac.Probe().WithInitialDelaySeconds(2).WithPeriodSeconds(5).WithFailureThreshold(3)
	if spec.HealthPath != "" {
		probe.WithHTTPGet(corev1ac.HTTPGetAction().WithPath(spec.HealthPath).WithPort(intstr.FromInt32(spec.Port)))
	} else {
		probe.WithTCPSocket(corev1ac.TCPSocketAction().WithPort(intstr.FromInt32(spec.Port)))
	}

	container := corev1ac.Container().
		WithName(containerName).
		WithImage(spec.Image).
		WithPorts(corev1ac.ContainerPort().WithName("http").WithContainerPort(spec.Port)).
		WithEnvFrom(corev1ac.EnvFromSource().WithSecretRef(corev1ac.SecretEnvSource().WithName(envSecretName(spec.Name)))).
		WithResources(corev1ac.ResourceRequirements().WithRequests(resources).WithLimits(resources)).
		WithReadinessProbe(probe)

	pod := corev1ac.PodTemplateSpec().
		WithLabels(labels(spec)).
		WithAnnotations(map[string]string{envHashAnnot: envHash(spec.Env)}).
		WithSpec(corev1ac.PodSpec().
			// Apps do not call the Kubernetes API, so they get no token.
			WithAutomountServiceAccountToken(false).
			WithContainers(container))

	return appsv1ac.Deployment(spec.Name, Namespace(spec.Project)).
		WithLabels(labels(spec)).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(spec.Replicas).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(selector(spec))).
			WithProgressDeadlineSeconds(progressDeadlineSeconds).
			// Never remove a serving pod before its replacement is ready.
			WithStrategy(appsv1ac.DeploymentStrategy().
				WithType(appsv1.RollingUpdateDeploymentStrategyType).
				WithRollingUpdate(appsv1ac.RollingUpdateDeployment().
					WithMaxUnavailable(intstr.FromInt32(0)).
					WithMaxSurge(intstr.FromInt32(1)))).
			WithTemplate(pod))
}

func service(spec deploy.Spec) *corev1ac.ServiceApplyConfiguration {
	return corev1ac.Service(spec.Name, Namespace(spec.Project)).
		WithLabels(labels(spec)).
		WithSpec(corev1ac.ServiceSpec().
			WithType(corev1.ServiceTypeClusterIP).
			WithSelector(selector(spec)).
			WithPorts(corev1ac.ServicePort().
				WithName("http").
				WithPort(servicePort).
				WithTargetPort(intstr.FromInt32(spec.Port))))
}

func ingress(spec deploy.Spec, class string) *netv1ac.IngressApplyConfiguration {
	annotations := map[string]string{}
	if spec.TLS {
		// spec.tls alone would not enable TLS on the router; these do.
		annotations["traefik.ingress.kubernetes.io/router.entrypoints"] = "websecure"
		annotations["traefik.ingress.kubernetes.io/router.tls"] = "true"
	} else {
		annotations["traefik.ingress.kubernetes.io/router.entrypoints"] = "web"
	}

	return netv1ac.Ingress(spec.Name, Namespace(spec.Project)).
		WithLabels(labels(spec)).
		WithAnnotations(annotations).
		WithSpec(netv1ac.IngressSpec().
			WithIngressClassName(class).
			WithRules(netv1ac.IngressRule().
				WithHost(spec.Host).
				WithHTTP(netv1ac.HTTPIngressRuleValue().
					WithPaths(netv1ac.HTTPIngressPath().
						WithPath("/").
						WithPathType(netv1.PathTypePrefix).
						WithBackend(netv1ac.IngressBackend().
							WithService(netv1ac.IngressServiceBackend().
								WithName(spec.Name).
								WithPort(netv1ac.ServiceBackendPort().WithNumber(servicePort))))))))
}

// statusOf reads a Deployment the way `kubectl rollout status` does: the
// controller must have seen the latest spec, every replica must be on it, and
// every replica must be ready.
func statusOf(dep *appsv1.Deployment) deploy.Status {
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	st := deploy.Status{
		Desired: desired,
		Updated: dep.Status.UpdatedReplicas,
		Ready:   dep.Status.ReadyReplicas,
	}

	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
			st.State, st.Reason, st.Message = deploy.StateFailed, c.Reason, c.Message
			return st
		}
	}

	switch {
	case dep.Status.ObservedGeneration < dep.Generation || dep.Status.UpdatedReplicas == 0 && desired > 0:
		st.State = deploy.StatePending
	case dep.Status.UpdatedReplicas == desired && dep.Status.Replicas == desired && dep.Status.AvailableReplicas == desired:
		st.State = deploy.StateReady
	default:
		st.State = deploy.StateRollingOut
	}
	return st
}
