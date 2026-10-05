package rest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/vorash/vora-cloud/pkg/auth"
	"github.com/vorash/vora-cloud/pkg/dashboard"
	"github.com/vorash/vora-cloud/pkg/githubapp"
)

type seenDeliveries map[string]bool

func (s seenDeliveries) Record(_ context.Context, id, _ string) (bool, error) {
	fresh := !s[id]
	s[id] = true
	return fresh, nil
}

// GitHub sends no session, no API key and no Origin. The real route table has
// authentication on /api/v1 and an origin check on the dashboard's writes, so
// the webhook has to get past both on the strength of its signature alone.
func TestGitHubWebhookIsReachableThroughTheRealRoutes(t *testing.T) {
	s := &Server{echo: echo.New()}
	s.setupRoutes()
	dash, err := dashboard.New(dashboard.Config{
		CSRFKey: bytes.Repeat([]byte{1}, 32), Limiter: auth.NewAttemptLimiter(time.Second, 1), PublicURL: "https://cloud.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	dash.Register(s.echo)
	hook, err := githubapp.NewWebhook("s3cret", seenDeliveries{})
	if err != nil {
		t.Fatal(err)
	}
	hook.Register(s.echo)

	body := []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	post := func(signature string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, githubapp.WebhookPath, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-GitHub-Delivery", "d-1")
		req.Header.Set("X-Hub-Signature-256", signature)
		rec := httptest.NewRecorder()
		s.echo.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("sha256=" + hex.EncodeToString(mac.Sum(nil))); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "accepted") {
		t.Errorf("signed delivery got %d %s", rec.Code, rec.Body)
	}
	// 401 from the handler, not from the API's own authentication.
	if rec := post("sha256=" + strings.Repeat("00", 32)); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid signature") {
		t.Errorf("wrongly signed delivery got %d %s", rec.Code, rec.Body)
	}
}
