//go:build live

package githubapp

import (
	"os"
	"testing"
)

// Checks the real credentials against GitHub with a read-only call (GET /app):
//
//	GITHUB_APP_CLIENT_ID=... GITHUB_APP_PRIVATE_KEY_FILE=... go test -tags live -run Live ./pkg/githubapp
//
// Run it after creating or rotating the App's key. It proves that GitHub
// accepts this client ID as the JWT issuer and this key as its signature.
func TestLiveGitHubAcceptsTheAppsJWT(t *testing.T) {
	id, file := os.Getenv("GITHUB_APP_CLIENT_ID"), os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE")
	if id == "" || file == "" {
		t.Skip("GITHUB_APP_CLIENT_ID and GITHUB_APP_PRIVATE_KEY_FILE are not set")
	}
	key, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApp(id, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := app.api.Apps.Get(t.Context(), "")
	if err != nil {
		t.Fatalf("GitHub refused the App's JWT: %v", err)
	}
	t.Logf("authenticated as the App %q (slug %q)", got.GetName(), got.GetSlug())
}
