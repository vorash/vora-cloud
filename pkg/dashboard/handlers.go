package dashboard

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vorash/vora-cloud/pkg/auth"
	"github.com/vorash/vora-cloud/pkg/project"
)

// page is what every template receives.
type page struct {
	Title string
	// User is the signed-in user's name; empty on the sign-in page.
	User  string
	CSRF  string
	Error string
	Data  any
}

func (d *Dashboard) render(c echo.Context, status int, name string, p page) error {
	if csrf, ok := c.Get(csrfKey).(string); ok {
		p.CSRF = csrf
		p.User = sessionOf(c).UserName
	}
	t, ok := d.pages[name]
	if !ok {
		return d.serverError(c, errors.New("no template "+name))
	}
	// Render to a buffer first so a template error cannot leave a half page.
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", p); err != nil {
		return d.serverError(c, err)
	}
	return c.HTMLBlob(status, buf.Bytes())
}

func (d *Dashboard) errorPage(c echo.Context, status int, msg string) error {
	return d.render(c, status, "error", page{Title: "Error", Error: msg})
}

// serverError logs the cause and shows a generic page.
func (d *Dashboard) serverError(c echo.Context, err error) error {
	log.Printf("dashboard %s %s: %v", c.Request().Method, c.Path(), err)
	c.Set(csrfKey, nil) // the session may be the problem; render as anonymous
	var buf bytes.Buffer
	if t, ok := d.pages["error"]; ok && t.ExecuteTemplate(&buf, "base", page{Title: "Error", Error: "Something went wrong. Try again."}) == nil {
		return c.HTMLBlob(http.StatusInternalServerError, buf.Bytes())
	}
	return c.String(http.StatusInternalServerError, "internal error")
}

func (d *Dashboard) notFound(c echo.Context) error {
	return d.errorPage(c, http.StatusNotFound, "Not found.")
}

func (d *Dashboard) loginForm(c echo.Context) error {
	return d.render(c, http.StatusOK, "login", page{Title: "Sign in"})
}

func (d *Dashboard) login(c echo.Context) error {
	ctx := c.Request().Context()
	email := strings.TrimSpace(c.FormValue("email"))
	if !d.cfg.Limiter.Allow(email) {
		return d.render(c, http.StatusTooManyRequests, "login", page{Title: "Sign in", Error: "Too many attempts. Wait a minute and try again."})
	}
	user, err := d.cfg.Users.AuthenticateUser(ctx, email, c.FormValue("password"))
	if errors.Is(err, auth.ErrInvalidCredentials) {
		return d.render(c, http.StatusUnauthorized, "login", page{Title: "Sign in", Error: "Invalid email or password."})
	}
	if err != nil {
		return d.serverError(c, err)
	}

	// A fresh token on every sign-in, so a token planted before it is useless.
	if old, err := c.Cookie(cookieName); err == nil {
		_ = d.cfg.Sessions.Delete(ctx, hashToken(old.Value))
	}
	token, err := newToken()
	if err != nil {
		return d.serverError(c, err)
	}
	if err := d.cfg.Sessions.Create(ctx, user.ID, hashToken(token), time.Now().Add(sessionLifetime)); err != nil {
		return d.serverError(c, err)
	}
	d.setSessionCookie(c, token)
	return c.Redirect(http.StatusSeeOther, "/")
}

func (d *Dashboard) logout(c echo.Context) error {
	if cookie, err := c.Cookie(cookieName); err == nil {
		if err := d.cfg.Sessions.Delete(c.Request().Context(), hashToken(cookie.Value)); err != nil {
			return d.serverError(c, err)
		}
	}
	clearSessionCookie(c)
	return c.Redirect(http.StatusSeeOther, "/login")
}

func (d *Dashboard) projects(c echo.Context) error {
	list, err := d.cfg.Projects.ListProjects(c.Request().Context(), sessionOf(c).UserID)
	if err != nil {
		return d.serverError(c, err)
	}
	return d.render(c, http.StatusOK, "projects", page{Title: "Projects", Data: list})
}

type newProjectData struct {
	Regions    []string
	Name, Slug string
	Region     string
}

func (d *Dashboard) newProjectForm(c echo.Context) error {
	return d.render(c, http.StatusOK, "project_new", page{Title: "New project", Data: newProjectData{Regions: d.cfg.Regions}})
}

func (d *Dashboard) createProject(c echo.Context) error {
	form := newProjectData{
		Regions: d.cfg.Regions, Name: strings.TrimSpace(c.FormValue("name")),
		Slug: strings.TrimSpace(c.FormValue("slug")), Region: c.FormValue("region"),
	}
	p, err := d.cfg.Projects.CreateProject(c.Request().Context(), sessionOf(c).UserID, project.CreateInput{
		Name: form.Name, Slug: form.Slug, Region: form.Region,
	})
	switch {
	case errors.Is(err, project.ErrInvalid), errors.Is(err, project.ErrSlugTaken):
		return d.render(c, http.StatusUnprocessableEntity, "project_new", page{Title: "New project", Error: err.Error(), Data: form})
	case err != nil:
		return d.serverError(c, err)
	}
	return c.Redirect(http.StatusSeeOther, "/projects/"+p.ID.String())
}

type projectData struct {
	Project     *project.Project
	Deployments []project.Deployment
}

// projectID reads the :id parameter; a malformed id is "not found", the same
// as a project of someone else.
func projectID(c echo.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return uuid.UUID{}, project.ErrNotFound
	}
	return id, nil
}

func (d *Dashboard) showProject(c echo.Context) error {
	return d.showProjectWith(c, http.StatusOK, "")
}

// showProjectWith renders the project page, optionally with an error above it.
func (d *Dashboard) showProjectWith(c echo.Context, status int, msg string) error {
	id, err := projectID(c)
	if err != nil {
		return d.notFound(c)
	}
	ctx := c.Request().Context()
	user := sessionOf(c).UserID
	p, err := d.cfg.Projects.GetProject(ctx, user, id)
	if errors.Is(err, project.ErrNotFound) {
		return d.notFound(c)
	}
	if err != nil {
		return d.serverError(c, err)
	}
	deps, err := d.cfg.Projects.ListDeployments(ctx, user, id)
	if err != nil {
		return d.serverError(c, err)
	}
	return d.render(c, status, "project", page{Title: p.Name, Error: msg, Data: projectData{Project: p, Deployments: deps}})
}

// afterDeploy answers a deploy or redeploy: back to the project page, or the
// same page with the reason it was refused.
func (d *Dashboard) afterDeploy(c echo.Context, id uuid.UUID, err error) error {
	switch {
	case errors.Is(err, project.ErrNotFound):
		return d.notFound(c)
	case errors.Is(err, project.ErrInvalid):
		return d.showProjectWith(c, http.StatusUnprocessableEntity, err.Error())
	case err != nil:
		return d.serverError(c, err)
	}
	return c.Redirect(http.StatusSeeOther, "/projects/"+id.String())
}

func (d *Dashboard) deploy(c echo.Context) error {
	id, err := projectID(c)
	if err != nil {
		return d.notFound(c)
	}
	_, err = d.cfg.Projects.Deploy(c.Request().Context(), sessionOf(c).UserID, id, project.DeployInput{
		Image: strings.TrimSpace(c.FormValue("image")),
	})
	return d.afterDeploy(c, id, err)
}

func (d *Dashboard) redeploy(c echo.Context) error {
	id, err := projectID(c)
	if err != nil {
		return d.notFound(c)
	}
	_, err = d.cfg.Projects.Redeploy(c.Request().Context(), sessionOf(c).UserID, id)
	return d.afterDeploy(c, id, err)
}
