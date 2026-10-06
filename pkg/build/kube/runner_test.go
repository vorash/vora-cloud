package kube

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vorash/vora-cloud/pkg/build"
)

const (
	token = "ghs_SECRETTOKENVALUE0123456789abcdef"
	image = "moby/buildkit:v0.33.1-rootless@sha256:f8a833b2de9d68e27f0815e4a737abdfaf8a2e4c615650557df11025101557b4"
	ns    = "vora-builds"
)

func newRunner(t *testing.T) (*Runner, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset()
	r, err := New(client, Config{Namespace: ns, Image: image})
	if err != nil {
		t.Fatal(err)
	}
	return r, client
}

func spec() build.Spec {
	return build.Spec{
		ID: uuid.NewString(), Repo: "octo/web", CommitSHA: strings.Repeat("a", 40), Context: "frontend", Dockerfile: "Dockerfile.prod",
		PushRef:  "registry.registry.svc.cluster.local:5000/web/web:aaaaaaaaaaaa",
		CacheRef: "registry.registry.svc.cluster.local:5000/web/web:buildcache",
		Token:    token,
	}
}

func jobOf(t *testing.T, c *fake.Clientset, id string) *batchv1.Job {
	t.Helper()
	j, err := c.BatchV1().Jobs(ns).Get(t.Context(), jobName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// The token reaches the build as a file of a Secret and nowhere else: not an
// argument (visible in the Job and in the process list), not the environment, not
// a label or an annotation.
func TestTheTokenIsInTheSecretAndNowhereInTheJob(t *testing.T) {
	r, c := newRunner(t)
	s := spec()
	if err := r.Start(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(jobOf(t, c, s.ID))
	if strings.Contains(string(job), token) {
		t.Errorf("the token is in the Job: %s", job)
	}
	secret, err := c.CoreV1().Secrets(ns).Get(t.Context(), secretName(s.ID), metav1.GetOptions{})
	if err != nil || string(secret.Data["token"]) != token {
		t.Fatalf("the secret: %+v, %v", secret, err)
	}
	if strings.Contains(strings.Join(mapValues(secret.Labels, secret.Annotations), " "), token) {
		t.Error("the token is in the Secret's labels or annotations")
	}
}

func mapValues(maps ...map[string]string) []string {
	var out []string
	for _, m := range maps {
		for k, v := range m {
			out = append(out, k, v)
		}
	}
	return out
}

func TestTheJobIsAnUnprivilegedRootlessBuildWithNoAccessToTheCluster(t *testing.T) {
	r, c := newRunner(t)
	s := spec()
	if err := r.Start(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	job := jobOf(t, c, s.ID)
	pod := job.Spec.Template.Spec
	ctr := pod.Containers[0]

	if len(pod.Containers) != 1 || ctr.Image != image {
		t.Errorf("containers: %d, image %q (must be the pinned one)", len(pod.Containers), ctr.Image)
	}
	sc := ctr.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 1000 || sc.RunAsGroup == nil || *sc.RunAsGroup != 1000 {
		t.Errorf("must run as uid and gid 1000, never root: %+v", sc)
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Error("the build is privileged")
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined ||
		sc.AppArmorProfile == nil || sc.AppArmorProfile.Type != corev1.AppArmorProfileTypeUnconfined {
		t.Errorf("rootless BuildKit needs seccomp and AppArmor unconfined, and the tested job set exactly that: %+v", sc)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the pod gets a service account token")
	}
	if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
		t.Error("the pod gets the cluster's services as environment variables")
	}
	if pod.HostNetwork || pod.HostPID || pod.HostIPC {
		t.Error("the build shares the host's namespaces")
	}
	for _, v := range pod.Volumes {
		if v.HostPath != nil {
			t.Errorf("volume %s is a host path", v.Name)
		}
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("a failed build must not be retried by the cluster: a retry would run it with the same token")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 900 {
		t.Errorf("deadline %v, want 900 s", job.Spec.ActiveDeadlineSeconds)
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy %s", pod.RestartPolicy)
	}
	// What the namespace's quota and limits expect: every resource bounded.
	for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
		if ctr.Resources.Limits.Name(res, "").IsZero() || ctr.Resources.Requests.Name(res, "").IsZero() {
			t.Errorf("no request or limit for %s", res)
		}
	}
	if got := ctr.Resources.Limits.Cpu().String(); got != "2" {
		t.Errorf("cpu limit %s", got)
	}
	if got := ctr.Resources.Limits.Memory().String(); got != "3Gi" {
		t.Errorf("memory limit %s", got)
	}
	// The Secret is mounted read-only, and is the only volume that holds anything.
	var mounted bool
	for _, m := range ctr.VolumeMounts {
		if m.Name == "git" {
			mounted = m.ReadOnly && m.MountPath == "/run/secrets/git"
		}
	}
	if !mounted {
		t.Errorf("the token's Secret is not mounted read-only at /run/secrets/git: %+v", ctr.VolumeMounts)
	}
	for _, e := range ctr.Env {
		if strings.Contains(e.Value, "ghs_") || e.ValueFrom != nil {
			t.Errorf("environment variable %s carries or reads a secret", e.Name)
		}
	}
	if job.Labels[buildLabel] != s.ID || job.Spec.Template.Labels[buildLabel] != s.ID {
		t.Errorf("labels %v / %v", job.Labels, job.Spec.Template.Labels)
	}
}

func TestTheBuildArguments(t *testing.T) {
	r, c := newRunner(t)
	s := spec()
	if err := r.Start(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	args := jobOf(t, c, s.ID).Spec.Template.Spec.Containers[0].Args
	want := []string{
		"build",
		"--frontend=dockerfile.v0",
		"--opt=context=https://github.com/octo/web.git#" + strings.Repeat("a", 40) + ":frontend",
		"--opt=filename=Dockerfile.prod",
		"--secret=id=GIT_AUTH_TOKEN,src=/run/secrets/git/token",
		"--progress=plain",
		"--output=type=image,name=registry.registry.svc.cluster.local:5000/web/web:aaaaaaaaaaaa,push=true,registry.insecure=true",
		"--export-cache=type=registry,ref=registry.registry.svc.cluster.local:5000/web/web:buildcache,mode=min,registry.insecure=true,ignore-error=true",
	}
	if strings.Join(args, "\n") != strings.Join(want, "\n") {
		t.Errorf("args:\n%s\nwant:\n%s", strings.Join(args, "\n"), strings.Join(want, "\n"))
	}

	// With a cache to use, the build imports it; without one it must not ask.
	s2 := spec()
	s2.ImportCache, s2.Context = true, ""
	if err := r.Start(t.Context(), s2); err != nil {
		t.Fatal(err)
	}
	args = jobOf(t, c, s2.ID).Spec.Template.Spec.Containers[0].Args
	if !strings.Contains(strings.Join(args, " "), "--import-cache=type=registry,ref=registry.registry.svc.cluster.local:5000/web/web:buildcache,registry.insecure=true") {
		t.Errorf("no cache import: %v", args)
	}
	if got := args[2]; got != "--opt=context=https://github.com/octo/web.git#"+strings.Repeat("a", 40) {
		t.Errorf("a build of the repository root has a subfolder in its context: %s", got)
	}
}

func TestAnInvalidSpecCreatesNothing(t *testing.T) {
	r, c := newRunner(t)
	s := spec()
	s.PushRef = "reg/web:1,push=false"
	if err := r.Start(t.Context(), s); err == nil {
		t.Fatal("Start accepted it")
	}
	if len(c.Actions()) != 0 {
		t.Errorf("the cluster was touched: %v", c.Actions())
	}
}

// A Secret with no Job would hold a token that nothing will ever delete.
func TestAFailedJobCreationRemovesTheSecret(t *testing.T) {
	r, c := newRunner(t)
	c.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("quota exceeded")
	})
	s := spec()
	if err := r.Start(t.Context(), s); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("Start = %v", err)
	}
	if _, err := c.CoreV1().Secrets(ns).Get(t.Context(), secretName(s.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the secret was left behind: %v", err)
	}
}

func TestStatus(t *testing.T) {
	r, c := newRunner(t)
	id := uuid.NewString()
	if res, err := r.Status(t.Context(), id); err != nil || res.State != build.Missing {
		t.Errorf("no job: %+v, %v", res, err)
	}
	if err := r.Start(t.Context(), build.Spec{ID: id, Repo: "o/r", CommitSHA: strings.Repeat("b", 40), Dockerfile: "Dockerfile",
		PushRef: "r:5000/a/a:1", CacheRef: "r:5000/a/a:buildcache", Token: token}); err != nil {
		t.Fatal(err)
	}
	if res, _ := r.Status(t.Context(), id); res.State != build.Running {
		t.Errorf("a new job: %+v", res)
	}

	set := func(f func(*batchv1.JobStatus)) {
		j := jobOf(t, c, id)
		f(&j.Status)
		if _, err := c.BatchV1().Jobs(ns).UpdateStatus(t.Context(), j, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	started := metav1.NewTime(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	set(func(s *batchv1.JobStatus) { s.StartTime = &started })
	if res, _ := r.Status(t.Context(), id); res.State != build.Running || !res.StartedAt.Equal(started.Time) {
		t.Errorf("a running job: %+v", res)
	}
	set(func(s *batchv1.JobStatus) { s.Succeeded = 1 })
	if res, _ := r.Status(t.Context(), id); res.State != build.Succeeded {
		t.Errorf("a succeeded job: %+v", res)
	}
	set(func(s *batchv1.JobStatus) {
		s.Succeeded, s.Failed = 0, 1
		s.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded", Message: "Job was active longer than specified deadline"}}
	})
	res, _ := r.Status(t.Context(), id)
	if res.State != build.Failed || !strings.Contains(res.Reason, "DeadlineExceeded") {
		t.Errorf("a failed job: %+v", res)
	}
}

func TestLogsAreTheEndOfThePodsLog(t *testing.T) {
	r, c := newRunner(t)
	id := uuid.NewString()
	if text, err := r.Logs(t.Context(), id, 100); err != nil || text != "" {
		t.Errorf("no pod: %q, %v", text, err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "build-pod", Namespace: ns, Labels: map[string]string{buildLabel: id}}}
	if _, err := c.CoreV1().Pods(ns).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if text, err := r.Logs(t.Context(), id, 100); err != nil || text != "fake logs" {
		t.Errorf("Logs = %q, %v", text, err)
	}
	if text, _ := r.Logs(t.Context(), id, 4); text != "logs" {
		t.Errorf("a cap of 4 bytes kept %q, want the end", text)
	}
}

func TestCleanupRemovesTheJobAndTheSecretAndMayBeRepeated(t *testing.T) {
	r, c := newRunner(t)
	s := spec()
	if err := r.Start(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.Cleanup(t.Context(), s.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.BatchV1().Jobs(ns).Get(t.Context(), jobName(s.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the job is still there: %v", err)
	}
	if _, err := c.CoreV1().Secrets(ns).Get(t.Context(), secretName(s.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the secret is still there: %v", err)
	}
}

// After a restart the builds that are not running are removed, and nothing that
// is not a build's.
func TestSweepRemovesOnlyBuildsThatAreNotKept(t *testing.T) {
	r, c := newRunner(t)
	kept, gone := spec(), spec()
	for _, s := range []build.Spec{kept, gone} {
		if err := r.Start(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	// A Secret whose Job is already gone, and objects that are not builds'.
	orphan := uuid.NewString()
	_, _ = c.CoreV1().Secrets(ns).Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName(orphan), Namespace: ns, Labels: labels(orphan)}}, metav1.CreateOptions{})
	_, _ = c.CoreV1().Secrets(ns).Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Namespace: ns}}, metav1.CreateOptions{})
	_, _ = c.BatchV1().Jobs(ns).Create(t.Context(), &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses-job", Namespace: ns}}, metav1.CreateOptions{})

	if err := r.Sweep(t.Context(), func(id string) bool { return id == kept.ID }); err != nil {
		t.Fatal(err)
	}
	has := func(kind, name string) bool {
		var err error
		switch kind {
		case "job":
			_, err = c.BatchV1().Jobs(ns).Get(t.Context(), name, metav1.GetOptions{})
		default:
			_, err = c.CoreV1().Secrets(ns).Get(t.Context(), name, metav1.GetOptions{})
		}
		return err == nil
	}
	for name, want := range map[string]bool{
		"job " + jobName(kept.ID): true, "secret " + secretName(kept.ID): true,
		"job " + jobName(gone.ID): false, "secret " + secretName(gone.ID): false,
		"secret " + secretName(orphan): false,
		"secret someone-elses":         true, "job someone-elses-job": true,
	} {
		kind, n, _ := strings.Cut(name, " ")
		if got := has(kind, n); got != want {
			t.Errorf("%s exists = %v, want %v", name, got, want)
		}
	}
}

func TestNewNeedsANamespaceAndAnImage(t *testing.T) {
	for _, cfg := range []Config{{}, {Namespace: ns}, {Image: image}} {
		if _, err := New(fake.NewClientset(), cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
}
