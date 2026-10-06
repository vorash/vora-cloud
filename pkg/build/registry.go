package build

import (
	"context"
	"fmt"
	"net/http"
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
