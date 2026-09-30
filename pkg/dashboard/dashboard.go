// Package dashboard serves the web UI: server-rendered pages with cookie
// sessions (ADR 004). It has no JavaScript and reuses project.Service, so the
// ownership rule is the same as in the REST and gRPC APIs.
package dashboard

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/vorash/vora-cloud/pkg/database"
	"github.com/vorash/vora-cloud/pkg/project"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets/app.css
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
}

const sessionLifetime = 12 * time.Hour

// Dashboard is the web UI.
type Dashboard struct {
	cfg   Config
	pages map[string]*template.Template
}

func New(cfg Config) (*Dashboard, error) {
	if len(cfg.CSRFKey) < 32 {
		return nil, errors.New("dashboard: CSRF key must be at least 32 bytes")
	}
	if cfg.Limiter == nil {
		return nil, errors.New("dashboard: a sign-in limiter is required")
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	pages := map[string]*template.Template{}
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, path := range names {
		name := strings.TrimSuffix(strings.TrimPrefix(path, "templates/"), ".html")
		if name == "base" {
			continue
		}
		t, err := template.New(name).ParseFS(templateFS, "templates/base.html", path)
		if err != nil {
			return nil, fmt.Errorf("dashboard: parse %s: %w", path, err)
		}
		pages[name] = t
	}
	return &Dashboard{cfg: cfg, pages: pages}, nil
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

	e.GET("/assets/app.css", d.stylesheet, d.secureHeaders)
	e.GET("/login", d.loginForm, public...)
	e.POST("/login", d.login, signIn...)

	e.POST("/logout", d.logout, private...)
	e.GET("/", d.projects, private...)
	e.GET("/projects/new", d.newProjectForm, private...)
	e.POST("/projects", d.createProject, private...)
	e.GET("/projects/:id", d.showProject, private...)
	e.POST("/projects/:id/deployments", d.deploy, private...)
	e.POST("/projects/:id/redeploy", d.redeploy, private...)
}

func (d *Dashboard) stylesheet(c echo.Context) error {
	css, err := assetFS.ReadFile("assets/app.css")
	if err != nil {
		return err
	}
	c.Response().Header().Set("Cache-Control", "public, max-age=3600")
	return c.Blob(http.StatusOK, "text/css; charset=utf-8", css)
}
