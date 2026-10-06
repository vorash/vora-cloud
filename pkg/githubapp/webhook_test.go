package githubapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
)

const secret = "test-webhook-secret"

// The fixtures are shaped like the payloads in GitHub's webhook documentation.
// They are written by hand, not recorded from a real delivery.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sign(key string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type fakeDeliveries struct {
	mu   sync.Mutex
	seen map[string]string // id -> event
	err  error
}

func (f *fakeDeliveries) Record(_ context.Context, id, event string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	if _, ok := f.seen[id]; ok {
		return false, nil
	}
	f.seen[id] = event
	return true, nil
}

func (f *fakeDeliveries) Forget(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.seen, id)
	return nil
}

type delivery struct {
	event, id, signature, contentType string
	body                              []byte
}

func newHook(t *testing.T) (*echo.Echo, *fakeDeliveries) {
	t.Helper()
	store := &fakeDeliveries{seen: map[string]string{}}
	hook, err := NewWebhook(secret, store)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	hook.Register(e)
	return e, store
}

// signed is a delivery the way GitHub sends it.
func signed(event, id string, body []byte) delivery {
	return delivery{event: event, id: id, signature: sign(secret, body), contentType: "application/json", body: body}
}

func (d delivery) send(e *echo.Echo) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, WebhookPath, bytes.NewReader(d.body))
	if d.contentType != "" {
		req.Header.Set("Content-Type", d.contentType)
	}
	if d.signature != "" {
		req.Header.Set("X-Hub-Signature-256", d.signature)
	}
	if d.event != "" {
		req.Header.Set("X-GitHub-Event", d.event)
	}
	if d.id != "" {
		req.Header.Set("X-GitHub-Delivery", d.id)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestSignedDeliveryIsAcceptedAndRecorded(t *testing.T) {
	e, store := newHook(t)
	rec := signed("push", "d-1", fixture(t, "push.json")).send(e)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"accepted"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if store.seen["d-1"] != "push" {
		t.Errorf("the delivery was not recorded: %v", store.seen)
	}
}

// Nothing the endpoint does may happen for a request that is not signed with
// the secret: not the event handling, and not the delivery record either.
func TestUnsignedOrBadlySignedDeliveriesAreRejected(t *testing.T) {
	body := fixture(t, "push.json")
	tampered := append([]byte{}, body...)
	tampered[len(tampered)/2] ^= 1

	for name, d := range map[string]delivery{
		"no signature":      {event: "push", id: "d", contentType: "application/json", body: body},
		"empty signature":   {event: "push", id: "d", signature: "sha256=", contentType: "application/json", body: body},
		"wrong secret":      {event: "push", id: "d", signature: sign("another-secret", body), contentType: "application/json", body: body},
		"body changed":      {event: "push", id: "d", signature: sign(secret, body), contentType: "application/json", body: tampered},
		"not hex":           {event: "push", id: "d", signature: "sha256=" + strings.Repeat("zz", 32), contentType: "application/json", body: body},
		"no sha256 prefix":  {event: "push", id: "d", signature: strings.TrimPrefix(sign(secret, body), "sha256="), contentType: "application/json", body: body},
		"sha1 only":         {event: "push", id: "d", signature: "sha1=" + hex.EncodeToString(make([]byte, 20)), contentType: "application/json", body: body},
		"truncated digest":  {event: "push", id: "d", signature: sign(secret, body)[:20], contentType: "application/json", body: body},
		"unsigned ping":     {event: "ping", id: "d", contentType: "application/json", body: fixture(t, "ping.json")},
		"unsigned unknown":  {event: "issues", id: "d", contentType: "application/json", body: []byte(`{}`)},
		"unsigned no event": {id: "d", contentType: "application/json", body: body},
	} {
		t.Run(name, func(t *testing.T) {
			e, store := newHook(t)
			rec := d.send(e)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("got %d %s, want 401", rec.Code, rec.Body)
			}
			if len(store.seen) != 0 {
				t.Errorf("an unsigned request was recorded: %v", store.seen)
			}
		})
	}
}

// A signed delivery whose id was already seen must not be handled twice, and
// the first one must not be affected by an unsigned request using its id.
func TestReplayedDeliveryIsHandledOnce(t *testing.T) {
	e, store := newHook(t)
	body := fixture(t, "push.json")

	forged := delivery{event: "push", id: "d-1", signature: "sha256=" + strings.Repeat("00", 32), contentType: "application/json", body: body}
	if rec := forged.send(e); rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged request got %d", rec.Code)
	}

	first := signed("push", "d-1", body).send(e)
	if !strings.Contains(first.Body.String(), `"accepted"`) {
		t.Fatalf("the real delivery was not accepted after a forged one used its id: %s", first.Body)
	}
	again := signed("push", "d-1", body).send(e)
	if again.Code != http.StatusOK || !strings.Contains(again.Body.String(), `"duplicate"`) {
		t.Errorf("replay got %d %s", again.Code, again.Body)
	}
	if len(store.seen) != 1 {
		t.Errorf("recorded %d deliveries, want 1", len(store.seen))
	}
}

// GitHub shows any non-2xx as a failed delivery, so events the App does not use
// are acknowledged, and not stored.
func TestUnknownEventIsAcknowledgedAndIgnored(t *testing.T) {
	e, store := newHook(t)
	rec := signed("issues", "d-1", []byte(`{"action":"opened"}`)).send(e)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ignored"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if len(store.seen) != 0 {
		t.Errorf("an ignored event was recorded: %v", store.seen)
	}
}

// GitHub sends a ping when the webhook is switched on.
func TestPing(t *testing.T) {
	e, _ := newHook(t)
	d := signed("ping", "", fixture(t, "ping.json")) // a ping needs no delivery id handling
	if rec := d.send(e); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pong"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

// Every event the App subscribes to is recorded, so each one is deduplicated.
func TestSubscribedEventsAreRecorded(t *testing.T) {
	for _, event := range []string{"push", "pull_request", "installation", "installation_repositories"} {
		t.Run(event, func(t *testing.T) {
			e, store := newHook(t)
			if rec := signed(event, "d-1", []byte(`{}`)).send(e); rec.Code != http.StatusOK {
				t.Fatalf("got %d %s", rec.Code, rec.Body)
			}
			if store.seen["d-1"] != event {
				t.Errorf("not recorded: %v", store.seen)
			}
		})
	}
}

func TestMalformedRequestsAreRefusedAfterTheSignature(t *testing.T) {
	body := fixture(t, "push.json")
	for name, tc := range map[string]struct {
		d    delivery
		want int
	}{
		"form-encoded":          {delivery{event: "push", id: "d", signature: sign(secret, body), contentType: "application/x-www-form-urlencoded", body: body}, http.StatusUnsupportedMediaType},
		"no content type":       {delivery{event: "push", id: "d", signature: sign(secret, body), body: body}, http.StatusUnsupportedMediaType},
		"no delivery id":        {signed("push", "", body), http.StatusBadRequest},
		"oversized delivery id": {signed("push", strings.Repeat("a", 129), body), http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			e, store := newHook(t)
			if rec := tc.d.send(e); rec.Code != tc.want {
				t.Errorf("got %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
			if len(store.seen) != 0 {
				t.Errorf("recorded %v", store.seen)
			}
		})
	}
}

// A failure to record must not look like success: GitHub would not redeliver,
// and the push would be lost.
func TestStoreFailureIsAServerError(t *testing.T) {
	e, store := newHook(t)
	store.err = errors.New("database down")
	if rec := signed("push", "d-1", fixture(t, "push.json")).send(e); rec.Code != http.StatusInternalServerError {
		t.Errorf("got %d %s, want 500", rec.Code, rec.Body)
	}
}

func TestOversizedPayloadIsRefused(t *testing.T) {
	e, store := newHook(t)
	body := bytes.Repeat([]byte("a"), maxPayload+1)
	rec := signed("push", "d-1", body).send(e)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("got %d, want 413", rec.Code)
	}
	if len(store.seen) != 0 {
		t.Errorf("recorded %v", store.seen)
	}
}

// With an empty key anyone can compute a valid signature, so the receiver
// cannot be built without a secret.
func TestEmptySecretIsRefused(t *testing.T) {
	if _, err := NewWebhook("", &fakeDeliveries{}); err == nil {
		t.Fatal("NewWebhook accepted an empty secret")
	}
}
