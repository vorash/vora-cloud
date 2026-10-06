package dashboard

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vorash/vora-cloud/pkg/project"
)

// envData is the environment page. It holds names only: a value is sealed by
// project.Service and never read back, so it cannot reach a page, and the form
// is not refilled with one after an error.
type envData struct {
	Project *project.Project
	Names   []string // sorted
	// Name refills the name field after a refused form.
	Name string
	// HasDeployment says whether Redeploy has an image to run again. A deployment
	// queued by a push has none yet.
	HasDeployment bool
}

// notices are the messages a redirect after an env change can ask for, by the
// "done" query value. Anything else shows nothing.
var notices = map[string]string{
	"saved":   "Variable saved. It applies on the next deployment.",
	"removed": "Variable removed. It stops applying on the next deployment.",
}

func (d *Dashboard) showEnv(c echo.Context) error {
	return d.showEnvWith(c, http.StatusOK, "", "")
}

// showEnvWith renders the page, optionally with an error above it and the name
// the person typed.
func (d *Dashboard) showEnvWith(c echo.Context, status int, msg, name string) error {
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
	names, err := d.cfg.Projects.EnvNames(ctx, user, id)
	if err != nil {
		return d.serverError(c, err)
	}
	sort.Strings(names)
	canRedeploy, err := d.cfg.Projects.CanRedeploy(ctx, user, id)
	if err != nil {
		return d.serverError(c, err)
	}
	data := envData{Project: p, Names: names, Name: name, HasDeployment: canRedeploy}
	return d.render(c, status, "env", page{Title: "Environment variables", Error: msg, Notice: notices[c.QueryParam("done")], Data: data})
}

func (d *Dashboard) setEnv(c echo.Context) error {
	id, err := projectID(c)
	if err != nil {
		return d.notFound(c)
	}
	name := strings.TrimSpace(c.FormValue("name"))
	err = d.cfg.Projects.SetEnv(c.Request().Context(), sessionOf(c).UserID, id, name, c.FormValue("value"))
	return d.afterEnv(c, id, name, "saved", err)
}

func (d *Dashboard) deleteEnv(c echo.Context) error {
	id, err := projectID(c)
	if err != nil {
		return d.notFound(c)
	}
	err = d.cfg.Projects.DeleteEnv(c.Request().Context(), sessionOf(c).UserID, id, strings.TrimSpace(c.FormValue("name")))
	return d.afterEnv(c, id, "", "removed", err)
}

// afterEnv answers a change: back to the page with a confirmation, or the same
// page with the reason it was refused.
func (d *Dashboard) afterEnv(c echo.Context, id uuid.UUID, name, done string, err error) error {
	switch {
	case errors.Is(err, project.ErrNotFound):
		return d.notFound(c)
	case errors.Is(err, project.ErrInvalid):
		return d.showEnvWith(c, http.StatusUnprocessableEntity, err.Error(), name)
	case err != nil:
		return d.serverError(c, err)
	}
	return c.Redirect(http.StatusSeeOther, "/projects/"+id.String()+"/env?done="+done)
}

// rollback deploys the image of an earlier deployment that became ready.
func (d *Dashboard) rollback(c echo.Context) error {
	id, err := projectID(c)
	if err != nil {
		return d.notFound(c)
	}
	target, err := uuid.Parse(c.FormValue("deployment"))
	if err != nil {
		return d.notFound(c)
	}
	dep, err := d.cfg.Projects.Rollback(c.Request().Context(), sessionOf(c).UserID, id, target)
	return d.afterDeploy(c, id, dep, err)
}
