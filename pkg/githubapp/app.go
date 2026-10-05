package githubapp

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/go-github/v92/github"
)

const (
	// jwtBackdate covers clock drift between this host and GitHub.
	jwtBackdate = 60 * time.Second
	// jwtLifetime stays under GitHub's limit of ten minutes from its own clock,
	// even if this host's clock runs up to a minute ahead.
	jwtLifetime = 9 * time.Minute
	// refreshMargin is how long before expiry a cached installation token is
	// replaced, so a caller never gets one that expires mid-request.
	refreshMargin = 5 * time.Minute
	// apiTimeout bounds every call to GitHub; the default client has none.
	apiTimeout = 15 * time.Second
)

// App authenticates as the GitHub App: it signs the short-lived JWT that
// identifies the App, and trades it for installation tokens. Neither the
// private key nor a token is logged or put in an error.
type App struct {
	clientID string
	key      *rsa.PrivateKey
	api      *github.Client
	now      func() time.Time

	mu     sync.Mutex
	tokens map[int64]installationToken
}

type installationToken struct {
	value   string
	expires time.Time
}

// NewApp returns the App for a client ID and the PEM of its private key. The
// key is parsed once; a malformed key or an empty client ID is an error at
// startup, not at the first webhook.
func NewApp(clientID string, privateKeyPEM []byte) (*App, error) {
	return newApp(clientID, privateKeyPEM, time.Now)
}

func newApp(clientID string, privateKeyPEM []byte, now func() time.Time, opts ...github.ClientOptionsFunc) (*App, error) {
	if clientID == "" {
		return nil, errors.New("githubapp: the App's client ID is empty")
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("githubapp: the App's private key: %w", err)
	}
	a := &App{clientID: clientID, key: key, now: now, tokens: map[int64]installationToken{}}

	// Every request carries a JWT signed at that moment: one lives ten minutes
	// at most, and a client built once would otherwise outlive it.
	httpClient := &http.Client{Timeout: apiTimeout, Transport: appTransport{app: a, next: http.DefaultTransport}}
	a.api, err = github.NewClient(append([]github.ClientOptionsFunc{github.WithHTTPClient(httpClient)}, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("githubapp: %w", err)
	}
	return a, nil
}

// signedJWT identifies the App to GitHub. The issuer is the App's client ID.
func (a *App) signedJWT() (string, error) {
	now := a.now()
	claims := jwt.RegisteredClaims{
		Issuer:    a.clientID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-jwtBackdate)),
		ExpiresAt: jwt.NewNumericDate(now.Add(jwtLifetime)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(a.key)
}

// appTransport authenticates each request as the App.
type appTransport struct {
	app  *App
	next http.RoundTripper
}

func (t appTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.app.signedJWT()
	if err != nil {
		return nil, errors.New("githubapp: signing the App's JWT failed")
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+token)
	return t.next.RoundTrip(req)
}

// InstallationToken returns a token that acts as one installation of the App.
// It is cached until shortly before it expires, and a failed request is never
// cached. The token must not be logged, or written into an image or a file.
func (a *App) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	if t, ok := a.cached(installationID); ok {
		return t, nil
	}

	// Not under the lock: a slow GitHub must not block other installations.
	// Two callers may both fetch; the second token simply replaces the first.
	tok, _, err := a.api.Apps.CreateInstallationToken(ctx, installationID, nil)
	if err != nil {
		return "", fmt.Errorf("githubapp: installation %d token: %w", installationID, err)
	}
	if tok.GetToken() == "" || tok.ExpiresAt == nil {
		return "", fmt.Errorf("githubapp: installation %d token: GitHub's answer had no token or expiry", installationID)
	}

	a.mu.Lock()
	a.tokens[installationID] = installationToken{value: tok.GetToken(), expires: tok.ExpiresAt.Time}
	a.mu.Unlock()
	return tok.GetToken(), nil
}

func (a *App) cached(installationID int64) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tokens[installationID]
	if !ok || !a.now().Add(refreshMargin).Before(t.expires) {
		return "", false
	}
	return t.value, true
}
