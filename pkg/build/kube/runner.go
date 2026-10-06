// Package kube runs builds as Kubernetes Jobs: one rootless BuildKit pod per
// build, in a namespace that a network policy and a quota confine.
package kube

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/vorash/vora-cloud/pkg/build"
)

const (
	buildLabel   = "vora.sh/build"
	managedByKey = "app.kubernetes.io/managed-by"
	containerCtr = "build"
	secretDir    = "/run/secrets/git"
	// maxLogRead bounds what is read from a pod's log before its end is kept.
	maxLogRead = 4 << 20
)

// Config tunes a Runner. The limits match the namespace's LimitRange and quota
// (vora-cloud-ansible's builder role): one build at a time.
type Config struct {
	// Namespace is where builds run.
	Namespace string
	// Image is the BuildKit image, pinned by digest.
	Image string
	// CPU, Memory and Disk are what one build may use.
	CPU, Memory, Disk string
	// Deadline is how long a Job may run before the cluster ends it.
	Deadline time.Duration
}

func (c Config) withDefaults() Config {
	if c.CPU == "" {
		c.CPU = "2"
	}
	if c.Memory == "" {
		c.Memory = "3Gi"
	}
	if c.Disk == "" {
		c.Disk = "10Gi"
	}
	if c.Deadline == 0 {
		c.Deadline = 15 * time.Minute
	}
	return c
}

// Runner implements build.Runner on one cluster.
type Runner struct {
	client kubernetes.Interface
	cfg    Config
}

var _ build.Runner = (*Runner)(nil)

func New(client kubernetes.Interface, cfg Config) (*Runner, error) {
	cfg = cfg.withDefaults()
	if cfg.Namespace == "" || cfg.Image == "" {
		return nil, fmt.Errorf("kube: a namespace and a BuildKit image are required")
	}
	return &Runner{client: client, cfg: cfg}, nil
}

// NewFromKubeconfig connects to the cluster a kubeconfig file points at.
func NewFromKubeconfig(path string, cfg Config) (*Runner, error) {
	if path == "" {
		return nil, fmt.Errorf("kube: kubeconfig path is required")
	}
	rc, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("kube: load kubeconfig %s: %w", path, err)
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return New(client, cfg)
}

func jobName(id string) string    { return "build-" + id }
func secretName(id string) string { return "build-" + id + "-git" }

func labels(id string) map[string]string {
	return map[string]string{managedByKey: "vora-cloud", buildLabel: id}
}

// Start creates the build's Secret, then its Job.
func (r *Runner) Start(ctx context.Context, s build.Spec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName(s.ID), Namespace: r.cfg.Namespace, Labels: labels(s.ID)},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte(s.Token)},
	}
	if _, err := r.client.CoreV1().Secrets(r.cfg.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("kube: creating the build's secret: %w", err)
	}
	if _, err := r.client.BatchV1().Jobs(r.cfg.Namespace).Create(ctx, r.job(s), metav1.CreateOptions{}); err != nil {
		_ = r.client.CoreV1().Secrets(r.cfg.Namespace).Delete(ctx, secretName(s.ID), metav1.DeleteOptions{})
		return fmt.Errorf("kube: creating the build's job: %w", err)
	}
	return nil
}

// job is the Job of one build. The token is in none of it: BuildKit reads it from
// a file of the mounted Secret.
func (r *Runner) job(s build.Spec) *batchv1.Job {
	var zero int32
	deadline := int64(r.cfg.Deadline / time.Second)
	ttl := int32(3600) // a backstop: the orchestrator removes the Job itself
	subdir := ""
	if s.Context != "" {
		subdir = ":" + s.Context
	}
	args := []string{
		"build",
		"--frontend=dockerfile.v0",
		"--opt=context=https://github.com/" + s.Repo + ".git#" + s.CommitSHA + subdir,
		"--opt=filename=" + s.Dockerfile,
		"--secret=id=GIT_AUTH_TOKEN,src=" + secretDir + "/token",
		"--progress=plain",
		"--output=type=image,name=" + s.PushRef + ",push=true,registry.insecure=true",
		"--export-cache=type=registry,ref=" + s.CacheRef + ",mode=min,registry.insecure=true,ignore-error=true",
	}
	if s.ImportCache {
		args = append(args, "--import-cache=type=registry,ref="+s.CacheRef+",registry.insecure=true")
	}
	unconfined := corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
	mode := int32(0o444)
	uid := int64(1000) // the image's user; never root
	disk := resource.MustParse(r.cfg.Disk)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName(s.ID), Namespace: r.cfg.Namespace, Labels: labels(s.ID)},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &zero,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(s.ID)},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					// A build has no business with the Kubernetes API, and a service
					// account token in the pod would be one more thing to read.
					AutomountServiceAccountToken: new(bool),
					EnableServiceLinks:           new(bool),
					Containers: []corev1.Container{{
						Name:    containerCtr,
						Image:   r.cfg.Image,
						Command: []string{"buildctl-daemonless.sh"},
						Args:    args,
						Env:     []corev1.EnvVar{{Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"}},
						// Rootless BuildKit needs seccomp and AppArmor unconfined, and
						// nothing else: not privileged, and not root.
						SecurityContext: &corev1.SecurityContext{
							RunAsUser:       &uid,
							RunAsGroup:      &uid,
							SeccompProfile:  &unconfined,
							AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:              resource.MustParse("500m"),
								corev1.ResourceMemory:           resource.MustParse("1Gi"),
								corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:              resource.MustParse(r.cfg.CPU),
								corev1.ResourceMemory:           resource.MustParse(r.cfg.Memory),
								corev1.ResourceEphemeralStorage: disk,
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "git", MountPath: secretDir, ReadOnly: true},
							{Name: "state", MountPath: "/home/user/.local/share/buildkit"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "git", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName(s.ID), DefaultMode: &mode}}},
						{Name: "state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &disk}}},
					},
				},
			},
		},
	}
}

func (r *Runner) Status(ctx context.Context, id string) (build.Result, error) {
	job, err := r.client.BatchV1().Jobs(r.cfg.Namespace).Get(ctx, jobName(id), metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return build.Result{State: build.Missing}, nil
	}
	if err != nil {
		return build.Result{}, fmt.Errorf("kube: reading the build's job: %w", err)
	}
	res := build.Result{State: build.Running}
	if job.Status.StartTime != nil {
		res.StartedAt = job.Status.StartTime.Time
	}
	switch {
	case job.Status.Succeeded > 0:
		res.State = build.Succeeded
	case job.Status.Failed > 0:
		res.State = build.Failed
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			res.State = build.Failed
			res.Reason = strings.TrimSpace(c.Reason + ": " + c.Message)
		}
	}
	return res, nil
}

func (r *Runner) Logs(ctx context.Context, id string, max int) (string, error) {
	pods, err := r.client.CoreV1().Pods(r.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: buildLabel + "=" + id})
	if err != nil {
		return "", fmt.Errorf("kube: finding the build's pod: %w", err)
	}
	if len(pods.Items) == 0 {
		return "", nil
	}
	tail := int64(5000)
	stream, err := r.client.CoreV1().Pods(r.cfg.Namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{Container: containerCtr, TailLines: &tail}).Stream(ctx)
	if errors.IsBadRequest(err) {
		return "", nil // the container has not started
	}
	if err != nil {
		return "", fmt.Errorf("kube: reading the build's log: %w", err)
	}
	defer func() { _ = stream.Close() }()
	data, err := io.ReadAll(io.LimitReader(stream, maxLogRead))
	if err != nil {
		return "", fmt.Errorf("kube: reading the build's log: %w", err)
	}
	if len(data) > max {
		data = data[len(data)-max:]
	}
	return string(data), nil
}

func (r *Runner) Cleanup(ctx context.Context, id string) error {
	background := metav1.DeletePropagationBackground
	var first error
	if err := r.client.BatchV1().Jobs(r.cfg.Namespace).Delete(ctx, jobName(id), metav1.DeleteOptions{PropagationPolicy: &background}); err != nil && !errors.IsNotFound(err) {
		first = fmt.Errorf("kube: removing the build's job: %w", err)
	}
	if err := r.client.CoreV1().Secrets(r.cfg.Namespace).Delete(ctx, secretName(id), metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) && first == nil {
		first = fmt.Errorf("kube: removing the build's secret: %w", err)
	}
	return first
}

// Sweep removes the Jobs and Secrets of builds that keep refuses. It touches only
// what carries the build label.
func (r *Runner) Sweep(ctx context.Context, keep func(id string) bool) error {
	sel := metav1.ListOptions{LabelSelector: buildLabel}
	jobs, err := r.client.BatchV1().Jobs(r.cfg.Namespace).List(ctx, sel)
	if err != nil {
		return fmt.Errorf("kube: listing builds: %w", err)
	}
	secrets, err := r.client.CoreV1().Secrets(r.cfg.Namespace).List(ctx, sel)
	if err != nil {
		return fmt.Errorf("kube: listing builds' secrets: %w", err)
	}
	ids := map[string]bool{}
	for _, j := range jobs.Items {
		ids[j.Labels[buildLabel]] = true
	}
	for _, s := range secrets.Items {
		ids[s.Labels[buildLabel]] = true
	}
	var first error
	for id := range ids {
		if id == "" || keep(id) {
			continue
		}
		if err := r.Cleanup(ctx, id); err != nil && first == nil {
			first = err
		}
	}
	return first
}
