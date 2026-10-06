package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vorash/vora-cloud/pkg/auth"
	"github.com/vorash/vora-cloud/pkg/githubapp"
	"github.com/vorash/vora-cloud/pkg/project"
)

// page is what every template receives.
type page struct {
	Title string
	// User is the signed-in user's name; empty on the sign-in page.
	User string
	CSRF string
	// Error is the reason a form was refused, shown in an alert above the page.
	Error string
	// Notice confirms a change, in an info bar above the page.
	Notice string
	// Code and Message are an error page's status and text.
	Code    int
	Message string
	// Home marks the projects list, which the navigation shows as the current page.
	Home bool
	// OnGitHub marks the GitHub page; GitHubNav (set by render) shows its link.
	OnGitHub  bool
	GitHubNav bool
	Data      any

	// Set by render. Shell is "app" (the navigation rail, signed in), "auth"
	// (sign-in) or "plain" (other signed-out pages); Asset and Script version the
	// stylesheet and the script.
	Shell  string
	Asset  string
	Script string
}

func (d *Dashboard) render(c echo.Context, status int, name string, p page) error {
	p.Asset, p.Script = d.versions["app.css"], d.versions["app.js"]
	p.GitHubNav = d.cfg.GitHub != nil
	if csrf, ok := c.Get(csrfKey).(string); ok {
		p.CSRF, p.User, p.Shell = csrf, sessionOf(c).UserName, "app"
	} else if name == "login" {
		p.Shell = "auth"
	} else {
		p.Shell = "plain"
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
	return d.render(c, status, "error", page{Title: statusTitle(status), Code: status, Message: msg})
}

// serverError logs the cause and shows a generic page.
func (d *Dashboard) serverError(c echo.Context, err error) error {
	log.Printf("dashboard %s %s: %v", c.Request().Method, c.Path(), err)
	c.Set(csrfKey, nil) // the session may be the problem; render as anonymous
	var buf bytes.Buffer
	p := page{Title: statusTitle(http.StatusInternalServerError), Code: http.StatusInternalServerError, Message: "Try again in a moment.", Shell: "plain", Asset: d.versions["app.css"], Script: d.versions["app.js"]}
	if t, ok := d.pages["error"]; ok && t.ExecuteTemplate(&buf, "base", p) == nil {
		return c.HTMLBlob(http.StatusInternalServerError, buf.Bytes())
	}
	return c.String(http.StatusInternalServerError, "internal error")
}

func (d *Dashboard) notFound(c echo.Context) error {
	return d.errorPage(c, http.StatusNotFound, "This page doesn't exist, or it belongs to another account.")
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
	list, err := d.cfg.Projects.ListSummaries(c.Request().Context(), sessionOf(c).UserID)
	if err != nil {
		return d.serverError(c, err)
	}
	return d.render(c, http.StatusOK, "projects", page{Title: "Projects", Home: true, Data: list})
}

type newProjectData struct {
	Regions    []string
	Name, Slug string
	Region     string
	// SlugTaken marks the slug field invalid after ErrSlugTaken.
	SlugTaken bool
	// Repo is the GitHub repository the project is created from; nil for a plain
	// project. GitHub says whether the GitHub page is offered at all.
	Repo   *repoChoice
	GitHub bool
	// Port is what the app listens on; Dockerfile and Context say how a linked
	// repository is built (the Dockerfile's path inside the context, and the
	// folder to build in).
	Port, Dockerfile, Context string
}

func (d *Dashboard) newProjectForm(c echo.Context) error {
	data := newProjectData{Regions: d.cfg.Regions, GitHub: d.cfg.GitHub != nil, Port: "80", Dockerfile: "Dockerfile"}
	installation, repo := c.QueryParam("installation"), c.QueryParam("repo")
	if d.cfg.GitHub == nil || (installation == "" && repo == "") {
		return d.render(c, http.StatusOK, "project_new", page{Title: "New project", Data: data})
	}
	choice, err := d.repoFor(c, installation, repo)
	switch {
	case errors.Is(err, githubapp.ErrNotFound):
		return d.notFound(c)
	case err != nil:
		data.Repo = choice
		return d.render(c, http.StatusBadGateway, "project_new", page{Title: "New project", Error: "GitHub did not answer. Try again in a moment.", Data: data})
	}
	data.Repo, data.Name, data.Slug = choice, path.Base(choice.FullName), slugFor(path.Base(choice.FullName))
	return d.render(c, http.StatusOK, "project_new", page{Title: "New project", Data: data})
}

func (d *Dashboard) createProject(c echo.Context) error {
	form := newProjectData{
		Regions: d.cfg.Regions, Name: strings.TrimSpace(c.FormValue("name")),
		Slug: strings.TrimSpace(c.FormValue("slug")), Region: c.FormValue("region"),
		GitHub: d.cfg.GitHub != nil, Port: strings.TrimSpace(c.FormValue("port")),
		Dockerfile: strings.TrimSpace(c.FormValue("dockerfile")), Context: strings.TrimSpace(c.FormValue("context")),
	}
	in := project.CreateInput{Name: form.Name, Slug: form.Slug, Region: form.Region}
	if form.Port != "" {
		port, err := strconv.ParseInt(form.Port, 10, 32)
		if err != nil || port < 1 || port > 65535 {
			return d.render(c, http.StatusUnprocessableEntity, "project_new", page{Title: "New project", Error: "The port must be a number between 1 and 65535.", Data: form})
		}
		in.Port = int32(port)
	}

	// Only the two ids come from the form. The repository is checked again, and
	// its name and branch are what GitHub says, never what was posted.
	if installation, repo := c.FormValue("installation"), c.FormValue("repo"); d.cfg.GitHub != nil && (installation != "" || repo != "") {
		choice, err := d.repoFor(c, installation, repo)
		switch {
		case errors.Is(err, githubapp.ErrNotFound):
			return d.notFound(c)
		case err != nil:
			form.Repo = choice
			return d.render(c, http.StatusBadGateway, "project_new", page{Title: "New project", Error: "GitHub did not answer. Try again in a moment.", Data: form})
		}
		form.Repo = choice
		in.Repo, in.DefaultBranch = choice.FullName, choice.DefaultBranch
		in.GitHubInstallationID, in.GitHubRepoID = choice.InstallationID, choice.ID
		// How the repository is built. The paths are checked by the service.
		if cfg, err := json.Marshal(project.BuildSettings{Dockerfile: form.Dockerfile, Context: form.Context}); err == nil {
			in.BuildConfig = cfg
		}
	}

	p, err := d.cfg.Projects.CreateProject(c.Request().Context(), sessionOf(c).UserID, in)
	switch {
	case errors.Is(err, project.ErrInvalid), errors.Is(err, project.ErrSlugTaken):
		form.SlugTaken = errors.Is(err, project.ErrSlugTaken)
		return d.render(c, http.StatusUnprocessableEntity, "project_new", page{Title: "New project", Error: err.Error(), Data: form})
	case err != nil:
		return d.serverError(c, err)
	}
	return c.Redirect(http.StatusSeeOther, "/projects/"+p.ID.String())
}

// deploymentsPerPage is how many deployments the project page lists at once.
const deploymentsPerPage = 20

type projectData struct {
	Project     *project.Project
	Deployments []project.Deployment // newest first, one page
	// Latest is the project's newest deployment, nil if there is none. It is
	// not always the first of Deployments: the page may be an older one.
	Latest *project.Deployment
	// Next is the cursor of the older page, empty on the last.
	Next string
	// Live is set while the newest deployment is still in progress; the page then
	// offers the event stream.
	Live bool
	// CanRedeploy is set when some deployment has an image to run again.
	CanRedeploy bool
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
	before := c.QueryParam("before")
	deps, err := d.cfg.Projects.ListDeployments(ctx, user, id, project.Page{Limit: deploymentsPerPage, Before: before})
	if errors.Is(err, project.ErrInvalid) { // a cursor that was edited by hand
		return d.notFound(c)
	}
	if err != nil {
		return d.serverError(c, err)
	}
	data := projectData{Project: p, Deployments: deps.Deployments, Next: deps.Next}
	switch {
	case before == "" && len(deps.Deployments) > 0:
		data.Latest = &deps.Deployments[0]
	case before != "":
		// An older page: the overview and Redeploy still go by the newest.
		newest, err := d.cfg.Projects.ListDeployments(ctx, user, id, project.Page{Limit: 1})
		if err != nil {
			return d.serverError(c, err)
		}
		if len(newest.Deployments) > 0 {
			data.Latest = &newest.Deployments[0]
		}
	}
	data.Live = data.Latest != nil && !data.Latest.Status.Final()
	if data.CanRedeploy, err = d.cfg.Projects.CanRedeploy(ctx, user, id); err != nil {
		return d.serverError(c, err)
	}
	return d.render(c, status, "project", page{Title: p.Name, Error: msg, Data: data})
}

// afterDeploy answers a deploy or redeploy: back to the project page at the new
// deployment's row, or the same page with the reason it was refused.
func (d *Dashboard) afterDeploy(c echo.Context, id uuid.UUID, dep *project.Deployment, err error) error {
	switch {
	case errors.Is(err, project.ErrNotFound):
		return d.notFound(c)
	case errors.Is(err, project.ErrInvalid):
		return d.showProjectWith(c, http.StatusUnprocessableEntity, err.Error())
	case err != nil:
		return d.serverError(c, err)
	}
	return c.Redirect(http.StatusSeeOther, "/projects/"+id.String()+"#deployment-"+dep.ID.String())
}

func (d *Dashboard) deploy(c echo.Context) error {
	id, err := projectID(c)
	if err != nil {
		return d.notFound(c)
	}
	dep, err := d.cfg.Projects.Deploy(c.Request().Context(), sessionOf(c).UserID, id, project.DeployInput{
		Image: strings.TrimSpace(c.FormValue("image")),
	})
	return d.afterDeploy(c, id, dep, err)
}

func (d *Dashboard) redeploy(c echo.Context) error {
	id, err := projectID(c)
	if err != nil {
		return d.notFound(c)
	}
	dep, err := d.cfg.Projects.Redeploy(c.Request().Context(), sessionOf(c).UserID, id)
	return d.afterDeploy(c, id, dep, err)
}
