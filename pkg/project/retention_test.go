package project_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	. "github.com/vorash/vora-cloud/pkg/project"
)

// image is what a build of a project pushes: its own repository, by digest. n is
// one hex digit that tells the images apart.
func image(slug, n string) string {
	return "localhost:5000/" + slug + "/" + slug + "@sha256:" + strings.Repeat(n, 64)
}

// ran runs an image as a deployment of the project and settles it in a status.
func (e *env) ran(t *testing.T, p *Project, img string, to Status) *Deployment {
	t.Helper()
	d := e.deploy(t, p, img)
	reason := ""
	if to == StatusFailed {
		reason = "did not become ready"
	}
	if err := e.store.Move(ctx, d.ID, StatusDeploying, to, reason); err != nil {
		t.Fatal(err)
	}
	return d
}

// remover records what it was asked to delete.
type remover struct {
	got  []string
	fail map[string]error
	on   func(image string)
}

func (r *remover) remove(_ context.Context, slug, image string) error {
	if r.on != nil {
		r.on(image)
	}
	if err := r.fail[image]; err != nil {
		return err
	}
	r.got = append(r.got, image)
	return nil
}

func (r *remover) removed(img string) bool {
	for _, g := range r.got {
		if g == img {
			return true
		}
	}
	return false
}

// A project keeps the images of its newest deployments and the one it serves;
// every older one is removed and can no longer be rolled back to.
func TestPruneRemovesTheOlderImagesAndRefusesTheirRollback(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	var deps []*Deployment
	for _, n := range []string{"1", "2", "3", "4", "5", "6", "7", "8"} {
		deps = append(deps, e.ran(t, p, image("web", n), StatusReady))
	}
	r := &remover{}
	n, err := e.svc.PruneImages(ctx, 3, r.remove)
	if err != nil || n != 5 {
		t.Fatalf("PruneImages = %d, %v; want 5", n, err)
	}
	for _, old := range []string{"1", "2", "3", "4", "5"} {
		if !r.removed(image("web", old)) {
			t.Errorf("image %s was kept", old)
		}
	}
	for _, kept := range []string{"6", "7", "8"} {
		if r.removed(image("web", kept)) {
			t.Errorf("image %s of one of the 3 newest deployments was removed", kept)
		}
	}
	// A rollback to an image that is gone is refused, and says why; one that is
	// kept still works.
	if _, err := e.svc.Rollback(ctx, e.alice, p.ID, deps[0].ID); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "removed") {
		t.Errorf("a rollback to a removed image: %v, want a refusal that says it was removed", err)
	}
	if _, err := e.svc.Rollback(ctx, e.alice, p.ID, deps[5].ID); err != nil {
		t.Errorf("a rollback to a kept image: %v", err)
	}
	// Nothing is left to do: a second pass removes nothing and asks for nothing.
	r2 := &remover{}
	if n, err := e.svc.PruneImages(ctx, 3, r2.remove); err != nil || n != 0 || len(r2.got) != 0 {
		t.Errorf("a second pass: %d, %v, %v", n, err, r2.got)
	}
}

// The deployment a project is serving is its newest ready one. If the newest
// deployments are failures, it is older than all of them, and it stays.
func TestPruneKeepsTheServingImageEvenWhenNewerDeploymentsFailed(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	e.ran(t, p, image("web", "1"), StatusReady) // serving
	for _, n := range []string{"2", "3", "4", "5", "6"} {
		e.ran(t, p, image("web", n), StatusFailed)
	}
	r := &remover{}
	if _, err := e.svc.PruneImages(ctx, 3, r.remove); err != nil {
		t.Fatal(err)
	}
	if r.removed(image("web", "1")) {
		t.Error("the image of the deployment being served was removed")
	}
	if len(r.got) != 2 || !r.removed(image("web", "2")) || !r.removed(image("web", "3")) {
		t.Errorf("removed %v, want only the two failures outside the newest three", r.got)
	}
}

// An image that is still being rolled out is never removed, however old.
func TestPruneKeepsTheImageOfADeploymentInProgress(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	old := e.ran(t, p, image("web", "1"), StatusReady)
	for _, n := range []string{"2", "3", "4", "5"} {
		e.ran(t, p, image("web", n), StatusReady)
	}
	// A new deployment supersedes the unfinished ones, so the old one is put back
	// in progress by hand.
	if err := e.store.Move(ctx, old.ID, StatusReady, StatusDeploying, ""); err != nil {
		t.Fatal(err)
	}
	r := &remover{}
	if _, err := e.svc.PruneImages(ctx, 2, r.remove); err != nil {
		t.Fatal(err)
	}
	if r.removed(image("web", "1")) {
		t.Error("the image of a deployment in progress was removed")
	}
}

// A rollback copies the image of an older deployment, so that deployment's
// image is the newest one's too: it must not go while the copy is among the kept.
func TestPruneKeepsAnImageThatAKeptDeploymentRuns(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	first := e.ran(t, p, image("web", "1"), StatusReady)
	e.ran(t, p, image("web", "2"), StatusReady)
	e.ran(t, p, image("web", "3"), StatusReady)
	back, err := e.svc.Rollback(ctx, e.alice, p.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Move(ctx, back.ID, StatusDeploying, StatusReady, ""); err != nil {
		t.Fatal(err)
	}
	r := &remover{}
	if _, err := e.svc.PruneImages(ctx, 1, r.remove); err != nil {
		t.Fatal(err)
	}
	if r.removed(image("web", "1")) {
		t.Error("an image that the newest deployment runs was removed because an older row of it is outside the newest")
	}
	if !r.removed(image("web", "2")) || !r.removed(image("web", "3")) {
		t.Errorf("removed %v, want images 2 and 3", r.got)
	}
}

// What a project keeps is its own: another project's deployments do not use up
// its places.
func TestPruneCountsEachProjectOnItsOwn(t *testing.T) {
	e := newEnv(t)
	a, b := e.project(t, e.alice, "aaa"), e.project(t, e.alice, "bbb")
	e.ran(t, a, image("aaa", "1"), StatusReady)
	for _, n := range []string{"1", "2", "3"} {
		e.ran(t, b, image("bbb", n), StatusReady)
	}
	r := &remover{}
	if _, err := e.svc.PruneImages(ctx, 2, r.remove); err != nil {
		t.Fatal(err)
	}
	if len(r.got) != 1 || r.got[0] != image("bbb", "1") {
		t.Errorf("removed %v, want only bbb's oldest", r.got)
	}
}

// If the registry fails, nothing is marked, and the next pass tries again. A
// mark made first would hide the image from every later pass.
func TestPruneMarksAnImageOnlyAfterItIsRemovedAndRetriesAFailure(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	e.ran(t, p, image("web", "1"), StatusReady)
	e.ran(t, p, image("web", "2"), StatusReady)
	r := &remover{fail: map[string]error{image("web", "1"): errors.New("registry down")}}
	if n, err := e.svc.PruneImages(ctx, 1, r.remove); err == nil || n != 0 {
		t.Fatalf("PruneImages = %d, %v; want the failure", n, err)
	}
	if rows, _ := e.store.ListImages(ctx); len(rows) != 2 {
		t.Errorf("a failed removal marked its image: %d rows still have one, want 2", len(rows))
	}
	r.fail = nil
	if n, err := e.svc.PruneImages(ctx, 1, r.remove); err != nil || n != 1 {
		t.Errorf("the retry: %d, %v", n, err)
	}
}

// An image that no build of the project made is left alone and not marked: the
// remover says so, and the pass goes on to the next one.
func TestPruneLeavesAnImageThatIsNotTheirsAndGoesOn(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	typed := e.ran(t, p, "registry.example.com/web:1", StatusReady)
	e.ran(t, p, image("web", "2"), StatusReady)
	e.ran(t, p, image("web", "3"), StatusReady)
	r := &remover{fail: map[string]error{"registry.example.com/web:1": ErrImageNotManaged}}
	n, err := e.svc.PruneImages(ctx, 1, r.remove)
	if err != nil || n != 1 || !r.removed(image("web", "2")) {
		t.Fatalf("PruneImages = %d, %v, %v", n, err, r.got)
	}
	if _, err := e.svc.Rollback(ctx, e.alice, p.ID, typed.ID); err != nil {
		t.Errorf("an image nobody removed was marked: %v", err)
	}
}

// The keep set is read again before each image: a rollback that lands while the
// pass runs copies an image the plan was about to remove.
func TestPruneReadsTheKeepSetAgainBeforeEachRemoval(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	first := e.ran(t, p, image("web", "1"), StatusReady)
	e.ran(t, p, image("web", "2"), StatusReady)
	e.ran(t, p, image("web", "3"), StatusReady)
	r := &remover{}
	// The plan is newest first: image 2, then image 1. While image 2 is removed, a
	// rollback to the older one starts.
	r.on = func(img string) {
		if img == image("web", "2") {
			if _, err := e.svc.Rollback(ctx, e.alice, p.ID, first.ID); err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := e.svc.PruneImages(ctx, 1, r.remove); err != nil {
		t.Fatal(err)
	}
	if r.removed(image("web", "1")) {
		t.Error("an image was removed after a rollback to it had been started")
	}
}

func TestPruneNeedsToKeepAtLeastOne(t *testing.T) {
	e := newEnv(t)
	p := e.project(t, e.alice, "web")
	e.ran(t, p, image("web", "1"), StatusReady)
	r := &remover{}
	if _, err := e.svc.PruneImages(ctx, 0, r.remove); !errors.Is(err, ErrInvalid) || len(r.got) != 0 {
		t.Errorf("keep 0: %v, removed %v", err, r.got)
	}
}
