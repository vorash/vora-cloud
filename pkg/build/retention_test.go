package build

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/project"
)

func digestOf(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// ran deploys an image the way a user would type it, and settles it as ready.
func (h *harness) ran(p *project.Project, img string) uuid.UUID {
	h.t.Helper()
	d, err := h.svc.Deploy(h.t.Context(), h.owner, p.ID, project.DeployInput{Image: img})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.Move(h.t.Context(), d.ID, project.StatusDeploying, project.StatusReady, ""); err != nil {
		h.t.Fatal(err)
	}
	return d.ID
}

// Retention deletes from a project's own repository by digest, and from nowhere
// else: the image of a deployment is whatever its user typed, so it can name
// another project's image, a tag, or an image from another registry.
func TestPruneDeletesOnlyTheProjectsOwnImagesByDigest(t *testing.T) {
	h := newHarness(t)
	h.orch.cfg.Keep = 1
	p := h.project("web", "{}")
	h.ran(p, "localhost:5000/web/web@"+digestOf("1")) // ours
	h.ran(p, "localhost:5000/bob/bob@"+digestOf("2")) // another project's repository
	h.ran(p, "localhost:5000/web/web:v1")             // a tag
	h.ran(p, "registry.example.com/web/web@"+digestOf("3"))
	h.ran(p, "localhost:5000/web/web@sha256:abc")
	h.ran(p, "localhost:5000/web/web@"+digestOf("4")+"/../../bob/bob") // trailing text
	h.ran(p, "localhost:5000/web/web@"+digestOf("9"))                  // the newest: kept
	h.orch.Prune(t.Context())
	if len(h.registry.deleted) != 1 || h.registry.deleted[0] != "web/web@"+digestOf("1") {
		t.Errorf("deleted %v, want only web/web@%.15s", h.registry.deleted, digestOf("1"))
	}
}

// Another project's images are not this project's to keep or lose.
func TestPruneNeverDeletesAnotherProjectsImageOnBehalfOfAnOldDeployment(t *testing.T) {
	h := newHarness(t)
	h.orch.cfg.Keep = 1
	mine, bobs := h.project("mine", "{}"), h.project("bobs", "{}")
	h.ran(bobs, "localhost:5000/bobs/bobs@"+digestOf("b"))
	h.ran(mine, "localhost:5000/bobs/bobs@"+digestOf("b")) // typed by someone else's user
	h.ran(mine, "localhost:5000/mine/mine@"+digestOf("1"))
	h.orch.Prune(t.Context())
	for _, d := range h.registry.deleted {
		if strings.HasPrefix(d, "bobs/") {
			t.Errorf("deleted %s on behalf of a project that does not own it", d)
		}
	}
}

// A failed deletion is not recorded, so the next pass tries again; a deletion that
// succeeds is, and the deployment can no longer be rolled back to.
func TestPruneRecordsOnlyWhatTheRegistryDeleted(t *testing.T) {
	h := newHarness(t)
	h.orch.cfg.Keep = 1
	p := h.project("web", "{}")
	old := h.ran(p, "localhost:5000/web/web@"+digestOf("1"))
	h.ran(p, "localhost:5000/web/web@"+digestOf("2"))

	h.registry.delErr = errors.New("registry down")
	h.orch.Prune(t.Context())
	if got := h.status(old); got.ImagePrunedAt != nil {
		t.Fatal("an image the registry did not delete was recorded as removed")
	}
	h.registry.delErr = nil
	h.orch.Prune(t.Context())
	if got := h.status(old); got.ImagePrunedAt == nil || len(h.registry.deleted) != 1 {
		t.Errorf("after the retry: pruned at %v, deleted %v", got.ImagePrunedAt, h.registry.deleted)
	}
}

// It runs every PruneEvery, not on every pass.
func TestPruneRunsOnItsOwnSchedule(t *testing.T) {
	h := newHarness(t)
	h.orch.cfg.Keep = 1
	now := time.Now()
	h.orch.now = func() time.Time { return now }
	p := h.project("web", "{}")
	h.ran(p, "localhost:5000/web/web@"+digestOf("1"))
	h.ran(p, "localhost:5000/web/web@"+digestOf("2"))
	h.orch.maybePrune(t.Context())
	if len(h.registry.deleted) != 1 {
		t.Fatalf("the first pass deleted %v", h.registry.deleted)
	}
	h.ran(p, "localhost:5000/web/web@"+digestOf("3"))
	now = now.Add(h.orch.cfg.PruneEvery - time.Second)
	h.orch.maybePrune(t.Context())
	if len(h.registry.deleted) != 1 {
		t.Errorf("it ran again before its interval: %v", h.registry.deleted)
	}
	now = now.Add(2 * time.Second)
	h.orch.maybePrune(t.Context())
	if len(h.registry.deleted) != 2 {
		t.Errorf("it did not run when its interval was over: %v", h.registry.deleted)
	}
}

func TestPruneKeepsFiveDeploymentsByDefault(t *testing.T) {
	if got := (Config{}).withDefaults(); got.Keep != 5 || got.PruneEvery != 10*time.Minute {
		t.Errorf("defaults: keep %d every %s, want 5 every 10m0s", got.Keep, got.PruneEvery)
	}
}

// buildAndFinish runs a push through to a successful build, and says what the
// cache tag points at when the build ends.
func buildAndFinish(h *harness, p *project.Project, c, cacheAfter string) {
	h.t.Helper()
	id := h.push(p, sha(c))
	h.tick()
	h.runner.results[id.String()] = Result{State: Succeeded}
	h.registry.digests["web/web:"+sha(c)[:12]] = digestOf(c)
	if cacheAfter != "" {
		h.registry.digests["web/web:"+cacheTag] = cacheAfter
	}
	h.tick()
}

// Every export moves the cache tag to a new manifest and leaves the old one
// untagged, where garbage collection would keep it and its layers for good.
func TestTheCacheManifestAnExportReplacedIsDeleted(t *testing.T) {
	h := newHarness(t)
	p := h.project("web", "{}")
	h.registry.digests["web/web:"+cacheTag] = digestOf("c")
	buildAndFinish(h, p, "a", digestOf("d"))
	if len(h.registry.deleted) != 1 || h.registry.deleted[0] != "web/web@"+digestOf("c") {
		t.Errorf("deleted %v, want the replaced cache manifest", h.registry.deleted)
	}
}

func TestTheCacheIsLeftAloneWhenTheBuildDidNotMoveIt(t *testing.T) {
	h := newHarness(t)
	p := h.project("web", "{}")
	h.registry.digests["web/web:"+cacheTag] = digestOf("c")
	buildAndFinish(h, p, "a", "") // the export failed, as ignore-error allows: the tag stays
	if len(h.registry.deleted) != 0 {
		t.Errorf("deleted %v, but the cache tag still points at it", h.registry.deleted)
	}
}

func TestAFirstBuildDeletesNothing(t *testing.T) {
	h := newHarness(t)
	buildAndFinish(h, h.project("web", "{}"), "a", digestOf("d")) // no cache before
	if len(h.registry.deleted) != 0 {
		t.Errorf("deleted %v on a first build", h.registry.deleted)
	}
}

// A build that failed has deployed nothing and replaced nothing it can be sure of.
func TestAFailedBuildDoesNotDeleteTheCache(t *testing.T) {
	h := newHarness(t)
	p := h.project("web", "{}")
	h.registry.digests["web/web:"+cacheTag] = digestOf("c")
	id := h.push(p, sha("a"))
	h.tick()
	h.registry.digests["web/web:"+cacheTag] = digestOf("d")
	h.runner.results[id.String()] = Result{State: Failed, Reason: "boom"}
	h.tick()
	if len(h.registry.deleted) != 0 {
		t.Errorf("deleted %v after a failed build", h.registry.deleted)
	}
}

func TestHTTPRegistryDelete(t *testing.T) {
	digest := digestOf("a")
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v2/web/web/manifests/" + digest:
			w.WriteHeader(http.StatusAccepted)
		case "/v2/gone/gone/manifests/" + digest:
			w.WriteHeader(http.StatusNotFound)
		case "/v2/off/off/manifests/" + digest:
			w.WriteHeader(http.StatusMethodNotAllowed)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	r := &HTTPRegistry{BaseURL: srv.URL + "/"}

	if err := r.Delete(t.Context(), "web/web", digest); err != nil {
		t.Errorf("a deleted manifest: %v", err)
	}
	if len(asked) != 1 || asked[0] != "DELETE /v2/web/web/manifests/"+digest {
		t.Errorf("asked %v", asked)
	}
	// One that is already gone is as good as deleted: a pass that was interrupted
	// after the registry answered must be able to finish.
	if err := r.Delete(t.Context(), "gone/gone", digest); err != nil {
		t.Errorf("an image that is already gone: %v", err)
	}
	if err := r.Delete(t.Context(), "off/off", digest); err == nil || !strings.Contains(err.Error(), "REGISTRY_STORAGE_DELETE_ENABLED") {
		t.Errorf("a registry that does not allow deleting: %v, want it named", err)
	}
	if err := r.Delete(t.Context(), "boom/boom", digest); err == nil {
		t.Error("a registry error was taken as success")
	}
	// What goes into the URL is checked first: nothing else is asked.
	asked = nil
	for _, c := range [][2]string{
		{"web/web", "latest"}, {"web/web", "sha256:abc"}, {"web/web", digest + "/x"},
		{"web", digest}, {"../web/web", digest}, {"web/web/extra", digest}, {"Web/web", digest}, {"web/web?x=1", digest}, {"", digest},
	} {
		if err := r.Delete(t.Context(), c[0], c[1]); err == nil {
			t.Errorf("Delete(%q, %q) was accepted", c[0], c[1])
		}
	}
	if len(asked) != 0 {
		t.Errorf("requests were made for refused names: %v", asked)
	}
}
