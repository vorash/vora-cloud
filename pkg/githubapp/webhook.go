// Package githubapp is the service's side of its GitHub App: the webhook
// endpoint and, later, authentication as the App.
package githubapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"
)

// WebhookPath is where GitHub is told to deliver events.
const WebhookPath = "/api/v1/webhooks/github"

// maxPayload is GitHub's own cap: it does not deliver a larger payload.
const maxPayload = 25 << 20

// events are the ones the App subscribes to. installation and
// installation_repositories arrive without a subscription. Anything else is
// acknowledged and dropped.
var events = map[string]bool{
	"push":                      true,
	"pull_request":              true,
	"installation":              true,
	"installation_repositories": true,
}

// Deliveries remembers the deliveries received.
type Deliveries interface {
	// Record stores a delivery and reports whether it was new.
	Record(ctx context.Context, id, event string) (bool, error)
	// Forget removes one, so GitHub's redelivery of it is handled as new.
	Forget(ctx context.Context, id string) error
}

// Push is a push to a repository's default branch.
type Push struct {
	InstallationID, RepoID int64
	RepoFullName           string
	Ref, CommitSHA         string
}

// PushHandler acts on a push. An error means it was not handled: the delivery is
// forgotten and the webhook answers 500, so that GitHub redelivers it.
type PushHandler interface {
	HandlePush(ctx context.Context, p Push) error
}

// Webhook receives GitHub's events. A request is trusted only if it carries a
// valid signature made with the shared secret.
type Webhook struct {
	secret     []byte
	deliveries Deliveries
	push       PushHandler
}

// NewWebhook returns the receiver. An empty secret is refused: an HMAC with an
// empty key can be computed by anyone, so it would accept any request.
func NewWebhook(secret string, deliveries Deliveries) (*Webhook, error) {
	if secret == "" {
		return nil, errors.New("githubapp: the webhook secret is empty")
	}
	return &Webhook{secret: []byte(secret), deliveries: deliveries}, nil
}

// WithPushHandler sets what happens to a push to a repository's default branch.
// Without one, a push is recorded and nothing more.
func (w *Webhook) WithPushHandler(h PushHandler) *Webhook {
	w.push = h
	return w
}

// Register adds the endpoint to e. It sits outside the authenticated /api/v1
// group on purpose: GitHub sends no credentials, only the signature.
func (w *Webhook) Register(e *echo.Echo) {
	e.POST(WebhookPath, w.receive)
}

type status struct {
	Status string `json:"status"`
}

func (w *Webhook) receive(c echo.Context) error {
	req := c.Request()

	// The signature covers the raw bytes, so read them once, bounded, and check
	// them before looking at anything else about the request.
	body, err := io.ReadAll(http.MaxBytesReader(c.Response(), req.Body, maxPayload))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return c.JSON(http.StatusRequestEntityTooLarge, status{"payload too large"})
		}
		return c.JSON(http.StatusBadRequest, status{"unreadable body"})
	}
	if !w.validSignature(req.Header.Get("X-Hub-Signature-256"), body) {
		log.Printf("github webhook: rejected a request with a missing or wrong signature")
		return c.JSON(http.StatusUnauthorized, status{"invalid signature"})
	}

	event := req.Header.Get("X-GitHub-Event")
	if event == "ping" {
		return c.JSON(http.StatusOK, status{"pong"})
	}
	if !events[event] {
		return c.JSON(http.StatusOK, status{"ignored"})
	}

	// Later steps read the payload as JSON; a form-encoded App would break them
	// silently, so refuse it now.
	if mt, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type")); mt != "application/json" {
		return c.JSON(http.StatusUnsupportedMediaType, status{"the webhook must send application/json"})
	}
	id := req.Header.Get("X-GitHub-Delivery")
	if id == "" || len(id) > 128 {
		return c.JSON(http.StatusBadRequest, status{"missing or invalid X-GitHub-Delivery"})
	}

	// Only a signed delivery is recorded; otherwise anyone could fill the table
	// with ids that real deliveries would later be dropped for. It is recorded
	// before it is handled, so two copies arriving at once are handled once, and
	// forgotten again if handling fails, so a redelivery is not swallowed.
	fresh, err := w.deliveries.Record(req.Context(), id, event)
	if err != nil {
		log.Printf("github webhook: recording delivery %s: %v", id, err)
		return c.JSON(http.StatusInternalServerError, status{"could not record the delivery"})
	}
	if !fresh {
		log.Printf("github webhook: delivery %s (%s) was already received", id, event)
		return c.JSON(http.StatusOK, status{"duplicate"})
	}
	log.Printf("github webhook: received %s delivery %s", event, id)

	if event == "push" && w.push != nil {
		if code, err := w.handlePush(req.Context(), body); err != nil {
			log.Printf("github webhook: push delivery %s: %v", id, err)
			if ferr := w.deliveries.Forget(req.Context(), id); ferr != nil {
				log.Printf("github webhook: forgetting delivery %s: %v", id, ferr)
			}
			return c.JSON(code, status{"could not handle the push"})
		}
	}
	return c.JSON(http.StatusOK, status{"accepted"})
}

var (
	// A commit id, as GitHub sends it: 40 hex digits, or 64 for a SHA-256 repository.
	commitRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	repoRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// errMalformedPush is a signed push whose payload is not one.
var errMalformedPush = errors.New("malformed push payload")

type pushPayload struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
	Repo    struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// handlePush forwards a push to the repository's default branch, and ignores
// anything else: a tag, another branch, a deleted branch. It returns the status
// to answer with when it fails.
func (w *Webhook) handlePush(ctx context.Context, body []byte) (int, error) {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return http.StatusBadRequest, fmt.Errorf("%w: %v", errMalformedPush, err)
	}
	if p.Repo.ID <= 0 || p.Installation.ID <= 0 || !repoRe.MatchString(p.Repo.FullName) || p.Repo.DefaultBranch == "" ||
		!strings.HasPrefix(p.Ref, "refs/") || !commitRe.MatchString(p.After) {
		return http.StatusBadRequest, errMalformedPush
	}
	if p.Deleted || strings.Trim(p.After, "0") == "" || p.Ref != "refs/heads/"+p.Repo.DefaultBranch {
		return http.StatusOK, nil
	}
	err := w.push.HandlePush(ctx, Push{
		InstallationID: p.Installation.ID, RepoID: p.Repo.ID, RepoFullName: p.Repo.FullName, Ref: p.Ref, CommitSHA: p.After,
	})
	if err != nil {
		return http.StatusInternalServerError, err
	}
	return http.StatusOK, nil
}

// validSignature checks X-Hub-Signature-256, "sha256=" and the hex HMAC-SHA256
// of the body, in constant time. The older sha1 header is not accepted.
func (w *Webhook) validSignature(header string, body []byte) bool {
	hexSum, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSum)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, w.secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
