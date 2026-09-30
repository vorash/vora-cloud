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
	"golang.org/x/time/rate"

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

// Register adds the dashboard's routes to e. They sit outside /api/v1.
func (d *Dashboard) Register(e *echo.Echo) {
	g := e.Group("", d.secureHeaders)
	g.GET("/assets/app.css", d.stylesheet)

	// Sign-in is limited per account, so guessing one password is slow and
	// the bcrypt work an attacker can cause is bounded.
	limit := middleware.RateLimiterWithConfig(middleware.RateLimiterConfig{
		Store: middleware.NewRateLimiterMemoryStoreWithConfig(middleware.RateLimiterMemoryStoreConfig{
			Rate: rate.Every(12 * time.Second), Burst: 5, ExpiresIn: 15 * time.Minute,
		}),
		IdentifierExtractor: func(c echo.Context) (string, error) {
			return strings.ToLower(strings.TrimSpace(c.FormValue("email"))), nil
		},
		DenyHandler: func(c echo.Context, _ string, _ error) error {
			return d.render(c, http.StatusTooManyRequests, "login", page{Title: "Sign in", Error: "Too many attempts. Wait a minute and try again."})
		},
	})
	g.GET("/login", d.loginForm)
	g.POST("/login", d.login, d.originCheck, limit)

	in := g.Group("", d.requireSession)
	in.POST("/logout", d.logout)
	in.GET("/", d.projects)
	in.GET("/projects/new", d.newProjectForm)
	in.POST("/projects", d.createProject)
	in.GET("/projects/:id", d.showProject)
	in.POST("/projects/:id/deployments", d.deploy)
	in.POST("/projects/:id/redeploy", d.redeploy)
}

func (d *Dashboard) stylesheet(c echo.Context) error {
	css, err := assetFS.ReadFile("assets/app.css")
	if err != nil {
		return err
	}
	c.Response().Header().Set("Cache-Control", "public, max-age=3600")
	return c.Blob(http.StatusOK, "text/css; charset=utf-8", css)
}
