package githubapp

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v92/github"
)

// CommitStatus is what Vora reports on a commit. State is pending, success,
// failure or error.
type CommitStatus struct {
	State       string
	Context     string
	Description string
	TargetURL   string
}

var statusStates = map[string]bool{"pending": true, "success": true, "failure": true, "error": true}

// maxDescription is GitHub's limit for a status description.
const maxDescription = 140

// PostStatus sets a status on a commit of a repository the installation can
// write statuses to ("Commit statuses" permission). The sha and the name come
// from a verified webhook, and are checked again here because they end up in a
// URL path.
func (a *App) PostStatus(ctx context.Context, installationID int64, repoFullName, sha string, s CommitStatus) error {
	owner, repo, ok := strings.Cut(repoFullName, "/")
	if !ok || !repoRe.MatchString(repoFullName) {
		return fmt.Errorf("githubapp: %q is not a repository name", repoFullName)
	}
	if !commitRe.MatchString(sha) {
		return fmt.Errorf("githubapp: %q is not a commit id", sha)
	}
	if !statusStates[s.State] {
		return fmt.Errorf("githubapp: %q is not a commit status state", s.State)
	}
	gh, err := a.InstallationClient(ctx, installationID)
	if err != nil {
		return err
	}
	if len(s.Description) > maxDescription {
		s.Description = s.Description[:maxDescription]
	}
	status := github.RepoStatus{State: &s.State, Context: &s.Context, Description: &s.Description}
	if s.TargetURL != "" {
		status.TargetURL = &s.TargetURL
	}
	if _, _, err := gh.Repositories.CreateStatus(ctx, owner, repo, sha, status); err != nil {
		return fmt.Errorf("githubapp: setting the status of %s@%.7s: %w", repoFullName, sha, err)
	}
	return nil
}
