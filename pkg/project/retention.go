package project

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
)

// ImageUse is a deployment's image as retention sees it.
type ImageUse struct {
	DeploymentID uuid.UUID
	ProjectID    uuid.UUID
	Slug         string
	Image        string
	Status       Status
	CreatedAt    time.Time
}

// ErrImageNotManaged is what a remover answers for an image that no build of this
// project made: one typed into "Deploy an image", a tag, another project's. It is
// left alone and not marked.
var ErrImageNotManaged = errors.New("project: not an image a build of this project made")

// prunable picks the images a project can lose. A project keeps, whatever their
// age: the images of its newest keep deployments, the one it is serving now (its
// newest ready deployment), and every deployment that is not final. Two
// deployments can run the same image (a rollback copies it), so an image is kept
// if any kept deployment runs it. The rest are returned once each.
func prunable(rows []ImageUse, keep int) []ImageUse {
	byProject := map[uuid.UUID][]ImageUse{}
	var order []uuid.UUID
	for _, r := range rows {
		if _, ok := byProject[r.ProjectID]; !ok {
			order = append(order, r.ProjectID)
		}
		byProject[r.ProjectID] = append(byProject[r.ProjectID], r)
	}
	var out []ImageUse
	for _, id := range order {
		rs := byProject[id]
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].CreatedAt.After(rs[j].CreatedAt) })
		kept, serving := map[string]bool{}, false
		for i, r := range rs {
			if i < keep || !r.Status.Final() {
				kept[r.Image] = true
			}
			if r.Status == StatusReady && !serving {
				kept[r.Image], serving = true, true
			}
		}
		seen := map[string]bool{}
		for _, r := range rs {
			if !kept[r.Image] && !seen[r.Image] {
				seen[r.Image] = true
				out = append(out, r)
			}
		}
	}
	return out
}

// PruneImages removes the images retention lets go of: remove deletes one from the
// registry, and only after it succeeds are the deployments that ran it marked, so
// a failure in between is tried again on the next pass (a registry that no longer
// has the image counts as removed). The keep set is read again before each image,
// so a rollback that just copied one is seen. It stops at the first failure.
func (s *Service) PruneImages(ctx context.Context, keep int, remove func(ctx context.Context, slug, image string) error) (int, error) {
	if keep < 1 {
		return 0, invalid("retention must keep at least one deployment's image")
	}
	rows, err := s.store.ListImages(ctx)
	if err != nil {
		return 0, err
	}
	pruned := 0
	for _, c := range prunable(rows, keep) {
		fresh, err := s.store.ListImages(ctx)
		if err != nil {
			return pruned, err
		}
		if !hasImage(prunable(fresh, keep), c) {
			continue
		}
		switch err := remove(ctx, c.Slug, c.Image); {
		case errors.Is(err, ErrImageNotManaged):
			continue
		case err != nil:
			return pruned, err
		}
		if err := s.store.MarkImagePruned(ctx, c.ProjectID, c.Image); err != nil {
			return pruned, err
		}
		pruned++
	}
	return pruned, nil
}

func hasImage(list []ImageUse, c ImageUse) bool {
	for _, r := range list {
		if r.ProjectID == c.ProjectID && r.Image == c.Image {
			return true
		}
	}
	return false
}
