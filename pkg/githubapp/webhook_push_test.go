package githubapp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
)

type fakePushHandler struct {
	mu     sync.Mutex
	pushes []Push
	err    error
}

func (f *fakePushHandler) HandlePush(_ context.Context, p Push) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes = append(f.pushes, p)
	return f.err
}

func newPushHook(t *testing.T) (*echo.Echo, *fakeDeliveries, *fakePushHandler) {
	t.Helper()
	store := &fakeDeliveries{seen: map[string]string{}}
	handler := &fakePushHandler{}
	hook, err := NewWebhook(secret, store)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	hook.WithPushHandler(handler).Register(e)
	return e, store, handler
}

// pushBody changes the default branch push fixture.
func pushBody(t *testing.T, replace ...string) []byte {
	t.Helper()
	body := string(fixture(t, "push-default-branch.json"))
	for i := 0; i < len(replace); i += 2 {
		if !strings.Contains(body, replace[i]) {
			t.Fatalf("the fixture has no %q", replace[i])
		}
		body = strings.Replace(body, replace[i], replace[i+1], 1)
	}
	return []byte(body)
}

func TestPushToTheDefaultBranchIsHandedOver(t *testing.T) {
	e, _, h := newPushHook(t)
	rec := signed("push", "d-1", fixture(t, "push-default-branch.json")).send(e)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"accepted"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	want := Push{InstallationID: 168273140, RepoID: 1350744260, RepoFullName: "octocat/invoicing-platform",
		Ref: "refs/heads/main", CommitSHA: "d8f1b0a4c91e3f0e4a2b7a9c5d6e8f1a2b3c4d5e"}
	if len(h.pushes) != 1 || h.pushes[0] != want {
		t.Errorf("pushes = %+v, want [%+v]", h.pushes, want)
	}
}

// Only a push to the branch the repository calls its default one is deployed.
func TestOtherPushesAreAcknowledgedAndIgnored(t *testing.T) {
	zeros := strings.Repeat("0", 40)
	for name, body := range map[string][]byte{
		"another branch": pushBody(t, `"refs/heads/main"`, `"refs/heads/feature"`),
		"a tag":          pushBody(t, `"refs/heads/main"`, `"refs/tags/v1.0.0"`),
		"a deleted branch": pushBody(t, `"after": "d8f1b0a4c91e3f0e4a2b7a9c5d6e8f1a2b3c4d5e"`, `"after": "`+zeros+`"`,
			`"deleted": false`, `"deleted": true`),
		"a deletion without the flag": pushBody(t, `"after": "d8f1b0a4c91e3f0e4a2b7a9c5d6e8f1a2b3c4d5e"`, `"after": "`+zeros+`"`),
		// The default branch is renamed on GitHub: the payload says what it is now.
		"the old default branch": pushBody(t, `"default_branch": "main"`, `"default_branch": "trunk"`),
	} {
		t.Run(name, func(t *testing.T) {
			e, store, h := newPushHook(t)
			if rec := signed("push", "d-1", body).send(e); rec.Code != http.StatusOK {
				t.Errorf("got %d %s, want 200", rec.Code, rec.Body)
			}
			if len(h.pushes) != 0 {
				t.Errorf("handed over: %+v", h.pushes)
			}
			if _, ok := store.seen["d-1"]; !ok {
				t.Error("the delivery was not recorded")
			}
		})
	}
}

func TestMalformedSignedPushesAreRefusedAndForgotten(t *testing.T) {
	for name, body := range map[string][]byte{
		"not JSON":              []byte(`{"ref": `),
		"no installation":       pushBody(t, `"installation": { "id": 168273140 }`, `"installation": {}`),
		"no repository id":      pushBody(t, `"id": 1350744260`, `"id": 0`),
		"a commit id too short": pushBody(t, `"after": "d8f1b0a4c91e3f0e4a2b7a9c5d6e8f1a2b3c4d5e"`, `"after": "d8f1b0a"`),
		"a commit id with a path": pushBody(t, `"after": "d8f1b0a4c91e3f0e4a2b7a9c5d6e8f1a2b3c4d5e"`,
			`"after": "../../../repos/x/y/statuses/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`),
		"a repository name with a space": pushBody(t, `"octocat/invoicing-platform"`, `"octocat/invoicing platform"`),
		"a repository name with a path":  pushBody(t, `"octocat/invoicing-platform"`, `"octocat/../admin"`),
		"no default branch":              pushBody(t, `"default_branch": "main"`, `"default_branch": ""`),
	} {
		t.Run(name, func(t *testing.T) {
			e, store, h := newPushHook(t)
			if rec := signed("push", "d-1", body).send(e); rec.Code != http.StatusBadRequest {
				t.Errorf("got %d %s, want 400", rec.Code, rec.Body)
			}
			if len(h.pushes) != 0 {
				t.Errorf("handed over: %+v", h.pushes)
			}
			if _, ok := store.seen["d-1"]; ok {
				t.Error("a refused delivery stayed recorded, so a corrected redelivery would be dropped")
			}
		})
	}
}

// If handling fails, GitHub must be able to redeliver, and the redelivery must be
// handled; once it has worked, a further copy must not be.
func TestAFailedPushIsRedeliverable(t *testing.T) {
	e, _, h := newPushHook(t)
	body := fixture(t, "push-default-branch.json")

	h.err = errors.New("database down")
	if rec := signed("push", "d-1", body).send(e); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "database") {
		t.Fatalf("got %d %s, want 500 without the cause", rec.Code, rec.Body)
	}
	h.err = nil
	if rec := signed("push", "d-1", body).send(e); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"accepted"`) {
		t.Fatalf("the redelivery: %d %s", rec.Code, rec.Body)
	}
	if rec := signed("push", "d-1", body).send(e); !strings.Contains(rec.Body.String(), `"duplicate"`) {
		t.Errorf("a copy after success: %s", rec.Body)
	}
	if len(h.pushes) != 2 {
		t.Errorf("handled %d times, want 2 (the failure and the redelivery, not the copy)", len(h.pushes))
	}
}

func TestOnlyPushEventsReachThePushHandler(t *testing.T) {
	e, _, h := newPushHook(t)
	for _, event := range []string{"installation", "pull_request", "installation_repositories"} {
		signed(event, "d-"+event, fixture(t, "push-default-branch.json")).send(e)
	}
	if len(h.pushes) != 0 {
		t.Errorf("handed over: %+v", h.pushes)
	}
}

func TestAPushWithNoHandlerIsJustRecorded(t *testing.T) {
	e, store := newHook(t) // no push handler
	rec := signed("push", "d-1", fixture(t, "push-default-branch.json")).send(e)
	if rec.Code != http.StatusOK || store.seen["d-1"] != "push" {
		t.Errorf("got %d, recorded %v", rec.Code, store.seen)
	}
}
