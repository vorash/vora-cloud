// Package githubapp is the service's side of its GitHub App: the webhook
// endpoint and, later, authentication as the App.
package githubapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
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
}

// Webhook receives GitHub's events. A request is trusted only if it carries a
// valid signature made with the shared secret.
type Webhook struct {
	secret     []byte
	deliveries Deliveries
}

// NewWebhook returns the receiver. An empty secret is refused: an HMAC with an
// empty key can be computed by anyone, so it would accept any request.
func NewWebhook(secret string, deliveries Deliveries) (*Webhook, error) {
	if secret == "" {
		return nil, errors.New("githubapp: the webhook secret is empty")
	}
	return &Webhook{secret: []byte(secret), deliveries: deliveries}, nil
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
	// with ids that real deliveries would later be dropped for.
	// TODO(#20): once an event is processed, record it after the work succeeds,
	// so a redelivery of a failed one is not swallowed as a duplicate.
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
	return c.JSON(http.StatusOK, status{"accepted"})
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
