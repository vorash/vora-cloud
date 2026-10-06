// Package dashboard serves the web UI: server-rendered pages with cookie
// sessions (ADR 004). Every page works without JavaScript; one first-party
// script only improves them. It reuses project.Service, so the ownership rule
// is the same as in the REST and gRPC APIs.
package dashboard

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/vorash/vora-cloud/pkg/database"
	"github.com/vorash/vora-cloud/pkg/project"
)

//go:embed templates/*.html
var templateFS embed.FS

// The stylesheet, the script, the two self-hosted fonts (with their licences)
// and the brand SVGs. Only the types in assetTypes are served.
//
//go:embed assets
var assetFS embed.FS

// SessionStore keeps sessions by the hash of their token.
type SessionStore interface {
	Create(ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time) error
	// Get returns nil for an unknown, expired or inactive-user session.
	Get(ctx context.Context, tokenHash string) (*database.Session, error)
	Delete(ctx context.Context, tokenHash string) error
}

// Authenticator checks an email and password.
type Authenticator interface {
	AuthenticateUser(ctx context.Context, email, password string) (*database.User, error)
}

// Config wires the dashboard to the services it uses.
type Config struct {
	Sessions SessionStore
	Users    Authenticator
	Projects *project.Service
	// Regions are the ids offered when creating a project.
	Regions []string
	// PublicURL is the site's origin, e.g. https://cloud.vora.sh. Writes are
	// accepted only from it. Empty derives it from each request, which is only
	// right when no proxy rewrites the host or scheme.
	PublicURL string
	// CSRFKey signs the per-session form tokens; at least 32 bytes.
	CSRFKey []byte
	// Limiter limits sign-in attempts per email.
	Limiter LoginLimiter
	// GitHub connects users' GitHub accounts. Nil switches the feature off: no
	// routes, no navigation entry.
	GitHub GitHub
	// Now is the clock behind "3 hours ago"; nil means time.Now.
	Now func() time.Time
}

const sessionLifetime = 12 * time.Hour

// Dashboard is the web UI.
type Dashboard struct {
	cfg   Config
	pages map[string]*template.Template
	now   func() time.Time
	// versions maps the stylesheet and the script to a hash of their content. It
	// is part of their URLs, so a new build is fetched at once and an unchanged
	// one is cached for a year.
	versions map[string]string

	// Live status (events.go). The fields are variables so tests can shorten them.
	pollEvery    time.Duration
	pingEvery    time.Duration
	maxStreamAge time.Duration
	maxStreams   int // open event streams per session
	streams      map[string]int
	streamsMu    sync.Mutex
	stop         chan struct{} // closed when the server shuts down
	stopOnce     sync.Once
}

func New(cfg Config) (*Dashboard, error) {
	if len(cfg.CSRFKey) < 32 {
		return nil, errors.New("dashboard: CSRF key must be at least 32 bytes")
	}
	if cfg.Limiter == nil {
		return nil, errors.New("dashboard: a sign-in limiter is required")
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	d := &Dashboard{
		cfg: cfg, now: cfg.Now, versions: map[string]string{},
		pollEvery: 3 * time.Second, pingEvery: 20 * time.Second, maxStreamAge: 10 * time.Minute, maxStreams: 4,
		streams: map[string]int{}, stop: make(chan struct{}),
	}
	for _, name := range []string{"app.css", "app.js"} {
		data, err := assetFS.ReadFile("assets/" + name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		d.versions[name] = hex.EncodeToString(sum[:4])
	}
	if d.now == nil {
		d.now = time.Now
	}
	pages := map[string]*template.Template{}
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, file := range names {
		name := strings.TrimSuffix(strings.TrimPrefix(file, "templates/"), ".html")
		if name == "base" {
			continue
		}
		t, err := template.New(name).Funcs(d.funcs()).ParseFS(templateFS, "templates/base.html", file)
		if err != nil {
			return nil, fmt.Errorf("dashboard: parse %s: %w", file, err)
		}
		pages[name] = t
	}
	d.pages = pages
	return d, nil
}

// LoginLimiter spends one sign-in attempt for an account. It is shared with
// the REST sign-in, so guesses over either count against one budget.
type LoginLimiter interface {
	Allow(key string) bool
}

// maxBody bounds a request body. Every form here is a few short fields, and
// the routes are reachable without a session.
const maxBody = "64K"

// Register adds the dashboard's routes to e. They sit outside /api/v1.
//
// Middleware is attached to each route, not to groups: echo answers every
// unmatched path under a group that has middleware with that middleware, so
// an empty-prefix group would turn any unknown URL on the server into a
// redirect to the sign-in page.
func (d *Dashboard) Register(e *echo.Echo) {
	public := []echo.MiddlewareFunc{d.secureHeaders, middleware.BodyLimit(maxBody)}
	private := append(append([]echo.MiddlewareFunc{}, public...), d.requireSession)
	signIn := append(append([]echo.MiddlewareFunc{}, public...), d.originCheck)

	e.GET("/assets/*", d.asset, d.secureHeaders)
	e.GET("/login", d.loginForm, public...)
	e.POST("/login", d.login, signIn...)

	e.POST("/logout", d.logout, private...)
	e.GET("/", d.projects, private...)
	e.GET("/projects/new", d.newProjectForm, private...)
	e.POST("/projects", d.createProject, private...)
	e.GET("/projects/:id", d.showProject, private...)
	e.POST("/projects/:id/deployments", d.deploy, private...)
	e.POST("/projects/:id/redeploy", d.redeploy, private...)
	e.POST("/projects/:id/rollback", d.rollback, private...)
	e.GET("/projects/:id/env", d.showEnv, private...)
	e.POST("/projects/:id/env", d.setEnv, private...)
	e.POST("/projects/:id/env/delete", d.deleteEnv, private...)
	e.GET("/projects/:id/events", d.events, private...)
	e.GET("/projects/:id/deployments/:dep/log", d.buildLog, private...)

	if d.cfg.GitHub != nil {
		e.GET("/github", d.githubIndex, private...)
		e.GET("/github/connect", d.githubConnect, private...)
		e.GET("/github/finish", d.githubFinish, private...)
		// Reached from github.com, so without the session cookie; see githubCallback.
		e.GET("/github/callback", d.githubCallback, public...)
	}

	// Streams do not end by themselves, and Shutdown does not cancel requests.
	e.Server.RegisterOnShutdown(func() { d.stopOnce.Do(func() { close(d.stop) }) })

	d.brandNotFound(e)
}

// assetTypes lists what /assets serves, with explicit types because the
// responses carry nosniff. The embedded licence texts are not served.
var assetTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".woff2": "font/woff2",
	".svg":   "image/svg+xml",
}

func (d *Dashboard) asset(c echo.Context) error {
	name := c.Param("*")
	ctype, ok := assetTypes[path.Ext(name)]
	if !ok {
		return echo.ErrNotFound
	}
	data, err := assetFS.ReadFile("assets/" + name) // rejects "..", absolute and directory paths
	if err != nil {
		return echo.ErrNotFound
	}
	cache := "public, max-age=86400"
	if version, ok := d.versions[name]; ok {
		cache = "no-cache"
		if c.QueryParam("v") == version {
			cache = "public, max-age=31536000, immutable"
		}
	}
	c.Response().Header().Set("Cache-Control", cache)
	return c.Blob(http.StatusOK, ctype, data)
}
