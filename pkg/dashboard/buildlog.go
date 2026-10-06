package dashboard

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vorash/vora-cloud/pkg/project"
)

// buildLog serves the log of a deployment's build as plain text. It is the
// owner's alone: a deployment of another user, or one that is not the project's,
// is "not found", the same as one that does not exist. Plain text, with nosniff
// from secureHeaders, so what a Dockerfile printed cannot become a page.
func (d *Dashboard) buildLog(c echo.Context) error {
	projectID, err := projectID(c)
	if err != nil {
		return d.notFound(c)
	}
	depID, err := uuid.Parse(c.Param("dep"))
	if err != nil {
		return d.notFound(c)
	}
	ctx := c.Request().Context()
	user := sessionOf(c).UserID
	dep, err := d.cfg.Projects.GetDeployment(ctx, user, depID)
	if errors.Is(err, project.ErrNotFound) || (err == nil && dep.ProjectID != projectID) {
		return d.notFound(c)
	}
	if err != nil {
		return d.serverError(c, err)
	}
	text, err := d.cfg.Projects.BuildLog(ctx, user, depID)
	if err != nil {
		return d.serverError(c, err)
	}
	if text == "" {
		text = "There is no build log for this deployment.\n"
	}
	return c.String(http.StatusOK, text)
}
