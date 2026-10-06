package build

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// HTTPRegistry asks a registry's HTTP API what it holds. The service runs on the
// registry's node, which publishes it on its loopback.
type HTTPRegistry struct {
	// BaseURL is the registry's address, such as http://127.0.0.1:5000.
	BaseURL string
	Client  *http.Client
}

// accept lists every manifest type a build can produce.
const accept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

func (r *HTTPRegistry) Digest(ctx context.Context, repo, tag string) (string, error) {
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, strings.TrimRight(r.BaseURL, "/")+"/v2/"+repo+"/manifests/"+tag, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", accept)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("build: asking the registry about %s:%s: %w", repo, tag, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNoImage
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("build: the registry answered %s for %s:%s", resp.Status, repo, tag)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", fmt.Errorf("build: the registry gave no usable digest for %s:%s", repo, tag)
	}
	return digest, nil
}

var (
	// A build's repository is <slug>/<slug>; nothing else is ever deleted.
	ownRepoRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?/[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	digestRe  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// Delete removes a manifest by digest, which the registry does for the tags that
// point at it too. The layers stay on the disk until the registry's garbage
// collection runs. It answers for nothing but a project's own repository and a
// digest, because both go into a URL.
func (r *HTTPRegistry) Delete(ctx context.Context, repo, digest string) error {
	if !ownRepoRe.MatchString(repo) || !digestRe.MatchString(digest) {
		return fmt.Errorf("build: refusing to delete %q@%q", repo, digest)
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimRight(r.BaseURL, "/")+"/v2/"+repo+"/manifests/"+digest, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("build: deleting %s@%.19s: %w", repo, digest, err)
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusNotFound:
		return nil
	case http.StatusMethodNotAllowed:
		return fmt.Errorf("build: the registry does not allow deleting images (REGISTRY_STORAGE_DELETE_ENABLED)")
	}
	return fmt.Errorf("build: the registry answered %s to deleting %s@%.19s", resp.Status, repo, digest)
}
