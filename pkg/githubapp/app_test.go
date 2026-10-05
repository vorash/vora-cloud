package githubapp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/go-github/v92/github"
)

const clientID = "Iv1.testclient"

func testKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// fakeGitHub answers POST /app/installations/{id}/access_tokens the way GitHub
// does: it accepts only a JWT signed by the App's key and returns a token that
// is valid for an hour.
type fakeGitHub struct {
	t   *testing.T
	pub *rsa.PublicKey
	now func() time.Time

	mu      sync.Mutex
	hits    map[string]int // installation id -> requests
	status  int            // non-zero: answer with this instead
	jwts    []string       // every bearer token seen
	counter int
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.jwts = append(f.jwts, bearer)

	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(bearer, &claims, func(*jwt.Token) (any, error) { return f.pub, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithTimeFunc(f.now), jwt.WithIssuer(clientID), jwt.WithIssuedAt(), jwt.WithExpirationRequired())
	if err != nil {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	// GitHub refuses a JWT that lives more than ten minutes from its own clock.
	if claims.ExpiresAt.Sub(f.now()) > 10*time.Minute {
		http.Error(w, `{"message":"'Expiration time' claim ('exp') must be a numeric value representing the future time at which the assertion expires"}`, http.StatusUnauthorized)
		return
	}

	// A host whose clock runs behind GitHub's would otherwise be refused: GitHub
	// rejects a JWT issued in its future.
	if f.now().Sub(claims.IssuedAt.Time) < 30*time.Second {
		http.Error(w, `{"message":"'Issued at' claim ('iat') must be an Integer representing the time that the assertion was issued"}`, http.StatusUnauthorized)
		return
	}

	var id string
	if _, err := fmt.Sscanf(r.URL.Path, "/app/installations/%s", &id); err != nil || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	id = strings.TrimSuffix(id, "/access_tokens")
	f.hits[id]++
	if f.status != 0 {
		http.Error(w, `{"message":"nope"}`, f.status)
		return
	}
	f.counter++
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"token":"ghs_%s_%d","expires_at":%q}`, id, f.counter, f.now().Add(time.Hour).UTC().Format(time.RFC3339))
}

type harness struct {
	app   *App
	gh    *fakeGitHub
	clock *time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	key, keyPEM := testKey(t)
	clock := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	h := &harness{clock: &clock}
	h.gh = &fakeGitHub{t: t, pub: &key.PublicKey, hits: map[string]int{}, now: func() time.Time { return *h.clock }}
	srv := httptest.NewServer(h.gh)
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	app, err := newApp(clientID, keyPEM, func() time.Time { return *h.clock }, github.WithURLs(&base, nil))
	if err != nil {
		t.Fatal(err)
	}
	h.app = app
	return h
}

// The fake server verifies the signature, the algorithm, the issuer and the
// lifetime, so a token it accepts is one GitHub would accept.
func TestInstallationTokenIsRequestedWithTheAppsJWT(t *testing.T) {
	h := newHarness(t)
	tok, err := h.app.InstallationToken(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_42_1" {
		t.Errorf("token = %q", tok)
	}
}

// A token is good for an hour; asking GitHub for a new one on every push would
// spend its rate limit and slow every deployment.
func TestInstallationTokenIsCached(t *testing.T) {
	h := newHarness(t)
	first, _ := h.app.InstallationToken(t.Context(), 42)
	*h.clock = h.clock.Add(30 * time.Minute)
	second, err := h.app.InstallationToken(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || h.gh.hits["42"] != 1 {
		t.Errorf("tokens %q then %q after %d request(s), want one request and the same token", first, second, h.gh.hits["42"])
	}
}

// A token handed out a minute before it expires would fail halfway through a
// clone or a status update. The numbers are literal on purpose: the token lives
// an hour, and the margin is a few minutes.
func TestInstallationTokenIsReplacedBeforeItExpires(t *testing.T) {
	h := newHarness(t)
	first, _ := h.app.InstallationToken(t.Context(), 42)

	*h.clock = h.clock.Add(54 * time.Minute) // six minutes left: still used
	if got, _ := h.app.InstallationToken(t.Context(), 42); got != first {
		t.Fatalf("a token with six minutes left was replaced")
	}
	*h.clock = h.clock.Add(2 * time.Minute) // four minutes left: replaced
	second, err := h.app.InstallationToken(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if second == first || h.gh.hits["42"] != 2 {
		t.Errorf("tokens %q then %q after %d request(s), want a new token", first, second, h.gh.hits["42"])
	}
}

// One installation's token must never be used for another: it would act on
// someone else's repositories.
func TestInstallationsGetSeparateTokens(t *testing.T) {
	h := newHarness(t)
	a, _ := h.app.InstallationToken(t.Context(), 1)
	b, _ := h.app.InstallationToken(t.Context(), 2)
	a2, _ := h.app.InstallationToken(t.Context(), 1)
	if a == b || a != a2 || h.gh.hits["1"] != 1 || h.gh.hits["2"] != 1 {
		t.Errorf("tokens %q %q %q, hits %v", a, b, a2, h.gh.hits)
	}
}

// Every request is signed when it is made, so a long-running service keeps
// working after the first JWT has expired.
func TestEachRequestCarriesAFreshJWT(t *testing.T) {
	h := newHarness(t)
	if _, err := h.app.InstallationToken(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	*h.clock = h.clock.Add(2 * time.Hour) // far past the first JWT's expiry
	if _, err := h.app.InstallationToken(t.Context(), 2); err != nil {
		t.Fatalf("a request two hours later was refused: %v", err)
	}
}

// A failure must not be remembered: the next call tries again, and no error
// may carry the JWT or a token.
func TestFailuresAreNotCachedAndLeakNothing(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newHarness(t)
			h.gh.status = status
			_, err := h.app.InstallationToken(t.Context(), 7)
			if err == nil {
				t.Fatal("expected an error")
			}
			h.gh.mu.Lock()
			jwts := append([]string{}, h.gh.jwts...)
			h.gh.mu.Unlock()
			for _, j := range jwts {
				if j != "" && strings.Contains(err.Error(), j) {
					t.Errorf("the error contains the JWT: %v", err)
				}
			}

			h.gh.mu.Lock()
			h.gh.status = 0
			h.gh.mu.Unlock()
			tok, err := h.app.InstallationToken(t.Context(), 7)
			if err != nil || tok == "" {
				t.Fatalf("the next call did not retry: %q, %v", tok, err)
			}
			if h.gh.hits["7"] != 2 {
				t.Errorf("hits = %d, want 2", h.gh.hits["7"])
			}
		})
	}
}

func TestNewAppRefusesBadCredentials(t *testing.T) {
	_, keyPEM := testKey(t)
	for name, tc := range map[string]struct {
		id  string
		pem []byte
	}{
		"empty client id": {"", keyPEM},
		"empty key":       {clientID, nil},
		"not a key":       {clientID, []byte("-----BEGIN RSA PRIVATE KEY-----\nnot a key\n-----END RSA PRIVATE KEY-----\n")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewApp(tc.id, tc.pem); err == nil {
				t.Error("NewApp accepted it")
			}
		})
	}
}

// A malformed key must not be echoed back in the startup error.
func TestNewAppErrorDoesNotEchoTheKey(t *testing.T) {
	_, err := NewApp(clientID, []byte("-----BEGIN RSA PRIVATE KEY-----\nc2VjcmV0LWtleS1tYXRlcmlhbA==\n-----END RSA PRIVATE KEY-----\n"))
	if err == nil || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Errorf("err = %v", err)
	}
}
