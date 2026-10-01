package rest

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vorash/vora-cloud/pkg/project"
)

// CreateProjectRequest JSON request for creating a project
type CreateProjectRequest struct {
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	Region        string `json:"region"`
	Repo          string `json:"repo,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Port          int32  `json:"port,omitempty"`
	Replicas      int32  `json:"replicas,omitempty"`
	CPUMillis     int64  `json:"cpu_millis,omitempty"`
	MemoryMiB     int64  `json:"memory_mib,omitempty"`
	HealthPath    string `json:"health_path,omitempty"`
}

// ProjectResponse JSON representation of a project
type ProjectResponse struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Slug          string    `json:"slug"`
	Repo          string    `json:"repo"`
	DefaultBranch string    `json:"default_branch"`
	Region        string    `json:"region"`
	Port          int32     `json:"port"`
	Replicas      int32     `json:"replicas"`
	CPUMillis     int64     `json:"cpu_millis"`
	MemoryMiB     int64     `json:"memory_mib"`
	HealthPath    string    `json:"health_path"`
	CreatedAt     time.Time `json:"created_at"`
}

// DeploymentResponse JSON representation of a deployment
type DeploymentResponse struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	CommitSHA  string     `json:"commit_sha"`
	Ref        string     `json:"ref"`
	Status     string     `json:"status"`
	Image      string     `json:"image"`
	Region     string     `json:"region"`
	URL        string     `json:"url"`
	Error      string     `json:"error"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// DeployImageRequest JSON request for deploying an existing image
type DeployImageRequest struct {
	Image     string `json:"image"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Ref       string `json:"ref,omitempty"`
}

// RollbackRequest JSON request for rolling back
type RollbackRequest struct {
	DeploymentID string `json:"deployment_id"`
}

// SetEnvRequest JSON request for setting an environment variable
type SetEnvRequest struct {
	Value string `json:"value"`
}

// EnvNamesResponse lists variable names; values are never returned
type EnvNamesResponse struct {
	Names []string `json:"names"`
}

func projectResponse(p *project.Project) ProjectResponse {
	return ProjectResponse{
		ID: p.ID.String(), Name: p.Name, Slug: p.Slug, Repo: p.Repo, DefaultBranch: p.DefaultBranch,
		Region: p.Region, Port: p.Port, Replicas: p.Replicas, CPUMillis: p.CPUMillis,
		MemoryMiB: p.MemoryMiB, HealthPath: p.HealthPath, CreatedAt: p.CreatedAt,
	}
}

func deploymentResponse(d *project.Deployment) DeploymentResponse {
	return DeploymentResponse{
		ID: d.ID.String(), ProjectID: d.ProjectID.String(), CommitSHA: d.CommitSHA, Ref: d.Ref,
		Status: string(d.Status), Image: d.Image, Region: d.Region, URL: d.URL, Error: d.Error,
		CreatedAt: d.CreatedAt, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
	}
}

// currentUser returns the authenticated user's id. Admin API keys have none.
func currentUser(c echo.Context) (uuid.UUID, bool) {
	s, ok := c.Get(UserContextKey).(string)
	if !ok {
		return uuid.UUID{}, false
	}
	id, err := uuid.Parse(s)
	return id, err == nil
}

// idParam reads a UUID path parameter. A malformed id is reported as missing,
// the same as an id that belongs to someone else.
func idParam(c echo.Context, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		return uuid.UUID{}, project.ErrNotFound
	}
	return id, nil
}

// projectError maps a service error to an HTTP response. Anything unexpected
// is logged and answered generically, so internals do not reach the client.
func projectError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, project.ErrNotFound):
		return c.JSON(http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, project.ErrSlugTaken):
		return c.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, project.ErrInvalid):
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		log.Printf("%s %s: %v", c.Request().Method, c.Path(), err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

// createProject handles POST /api/v1/projects
// @Summary Create a project
// @Description Create a project owned by the caller. The slug names the project's namespace and host, so it is unique across all users.
// @Tags projects
// @Accept json
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param request body CreateProjectRequest true "Create Project Request"
// @Success 201 {object} ProjectResponse
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 409 {object} map[string]string
// @Router /projects [post]
func (s *Server) createProject(c echo.Context) error {
	user, _ := currentUser(c)
	var req CreateProjectRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
	}
	p, err := s.projects.CreateProject(c.Request().Context(), user, project.CreateInput{
		Name: req.Name, Slug: req.Slug, Region: req.Region, Repo: req.Repo, DefaultBranch: req.DefaultBranch,
		Port: req.Port, Replicas: req.Replicas, CPUMillis: req.CPUMillis, MemoryMiB: req.MemoryMiB,
		HealthPath: req.HealthPath,
	})
	if err != nil {
		return projectError(c, err)
	}
	return c.JSON(http.StatusCreated, projectResponse(p))
}

// listProjects handles GET /api/v1/projects
// @Summary List projects
// @Description List the caller's projects
// @Tags projects
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Success 200 {array} ProjectResponse
// @Failure 403 {object} map[string]string
// @Router /projects [get]
func (s *Server) listProjects(c echo.Context) error {
	user, _ := currentUser(c)
	list, err := s.projects.ListProjects(c.Request().Context(), user)
	if err != nil {
		return projectError(c, err)
	}
	out := make([]ProjectResponse, len(list))
	for i := range list {
		out[i] = projectResponse(&list[i])
	}
	return c.JSON(http.StatusOK, out)
}

// getProject handles GET /api/v1/projects/:id
// @Summary Get a project
// @Tags projects
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path string true "Project ID"
// @Success 200 {object} ProjectResponse
// @Failure 404 {object} map[string]string
// @Router /projects/{id} [get]
func (s *Server) getProject(c echo.Context) error {
	user, _ := currentUser(c)
	id, err := idParam(c, "id")
	if err != nil {
		return projectError(c, err)
	}
	p, err := s.projects.GetProject(c.Request().Context(), user, id)
	if err != nil {
		return projectError(c, err)
	}
	return c.JSON(http.StatusOK, projectResponse(p))
}

// deleteProject handles DELETE /api/v1/projects/:id
// @Summary Delete a project
// @Description Tear down the project's namespace in its region, then delete the project and its deployments
// @Tags projects
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path string true "Project ID"
// @Success 204
// @Failure 404 {object} map[string]string
// @Router /projects/{id} [delete]
func (s *Server) deleteProject(c echo.Context) error {
	user, _ := currentUser(c)
	id, err := idParam(c, "id")
	if err != nil {
		return projectError(c, err)
	}
	if err := s.projects.DeleteProject(c.Request().Context(), user, id); err != nil {
		return projectError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// listDeployments handles GET /api/v1/projects/:id/deployments
// @Summary List a project's deployments
// @Description Newest first. Returns up to `limit` deployments (default 50, at most 100). When older ones remain, the X-Next-Cursor response header holds the `before` value that fetches them.
// @Tags deployments
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path string true "Project ID"
// @Param limit query int false "Most deployments to return"
// @Param before query string false "X-Next-Cursor of the previous page"
// @Success 200 {array} DeploymentResponse
// @Header 200 {string} X-Next-Cursor "Pass as `before` for older deployments; absent on the last page"
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Router /projects/{id}/deployments [get]
func (s *Server) listDeployments(c echo.Context) error {
	user, _ := currentUser(c)
	id, err := idParam(c, "id")
	if err != nil {
		return projectError(c, err)
	}
	var limit int
	if v := c.QueryParam("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "limit must be a number"})
		}
	}
	page, err := s.projects.ListDeployments(c.Request().Context(), user, id, project.Page{Limit: limit, Before: c.QueryParam("before")})
	if err != nil {
		return projectError(c, err)
	}
	if page.Next != "" {
		c.Response().Header().Set("X-Next-Cursor", page.Next)
	}
	out := make([]DeploymentResponse, len(page.Deployments))
	for i := range page.Deployments {
		out[i] = deploymentResponse(&page.Deployments[i])
	}
	return c.JSON(http.StatusOK, out)
}

// getDeployment handles GET /api/v1/deployments/:id
// @Summary Get a deployment
// @Tags deployments
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path string true "Deployment ID"
// @Success 200 {object} DeploymentResponse
// @Failure 404 {object} map[string]string
// @Router /deployments/{id} [get]
func (s *Server) getDeployment(c echo.Context) error {
	user, _ := currentUser(c)
	id, err := idParam(c, "id")
	if err != nil {
		return projectError(c, err)
	}
	d, err := s.projects.GetDeployment(c.Request().Context(), user, id)
	if err != nil {
		return projectError(c, err)
	}
	return c.JSON(http.StatusOK, deploymentResponse(d))
}

// deployImage handles POST /api/v1/projects/:id/deployments
// @Summary Deploy an image
// @Description Run an existing image for the project as a new deployment. The response is the deployment record; poll it until its status is ready or failed.
// @Tags deployments
// @Accept json
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path string true "Project ID"
// @Param request body DeployImageRequest true "Deploy Image Request"
// @Success 201 {object} DeploymentResponse
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Router /projects/{id}/deployments [post]
func (s *Server) deployImage(c echo.Context) error {
	user, _ := currentUser(c)
	id, err := idParam(c, "id")
	if err != nil {
		return projectError(c, err)
	}
	var req DeployImageRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
	}
	d, err := s.projects.Deploy(c.Request().Context(), user, id, project.DeployInput{
		Image: req.Image, CommitSHA: req.CommitSHA, Ref: req.Ref,
	})
	if err != nil {
		return projectError(c, err)
	}
	return c.JSON(http.StatusCreated, deploymentResponse(d))
}

// redeploy handles POST /api/v1/projects/:id/redeploy
// @Summary Redeploy a project
// @Description Run the project's latest image again, which also applies changed environment variables
// @Tags deployments
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path string true "Project ID"
// @Success 201 {object} DeploymentResponse
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Router /projects/{id}/redeploy [post]
func (s *Server) redeploy(c echo.Context) error {
	user, _ := currentUser(c)
	id, err := idParam(c, "id")
	if err != nil {
		return projectError(c, err)
	}
	d, err := s.projects.Redeploy(c.Request().Context(), user, id)
	if err != nil {
		return projectError(c, err)
	}
	return c.JSON(http.StatusCreated, deploymentResponse(d))
}

// rollback handles POST /api/v1/projects/:id/rollback
// @Summary Roll back to an earlier deployment
// @Description Deploy the image of an earlier deployment that became ready, as a new deployment
// @Tags deployments
// @Accept json
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path string true "Project ID"
// @Param request body RollbackRequest true "Rollback Request"
// @Success 201 {object} DeploymentResponse
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Router /projects/{id}/rollback [post]
func (s *Server) rollback(c echo.Context) error {
	user, _ := currentUser(c)
	id, err := idParam(c, "id")
	if err != nil {
		return projectError(c, err)
	}
	var req RollbackRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
	}
	target, err := uuid.Parse(req.DeploymentID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "deployment_id must be a deployment id"})
	}
	d, err := s.projects.Rollback(c.Request().Context(), user, id, target)
	if err != nil {
		return projectError(c, err)
	}
	return c.JSON(http.StatusCreated, deploymentResponse(d))
}

// listEnv handles GET /api/v1/projects/:id/env
// @Summary List environment variable names
// @Description Values are never returned
// @Tags projects
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path string true "Project ID"
// @Success 200 {object} EnvNamesResponse
// @Failure 404 {object} map[string]string
// @Router /projects/{id}/env [get]
func (s *Server) listEnv(c echo.Context) error {
	user, _ := currentUser(c)
	id, err := idParam(c, "id")
	if err != nil {
		return projectError(c, err)
	}
	names, err := s.projects.EnvNames(c.Request().Context(), user, id)
	if err != nil {
		return projectError(c, err)
	}
	return c.JSON(http.StatusOK, EnvNamesResponse{Names: names})
}

// setEnv handles PUT /api/v1/projects/:id/env/:name
// @Summary Set an environment variable
// @Description Stored encrypted; it applies on the next deployment or redeploy
// @Tags projects
// @Accept json
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path string true "Project ID"
// @Param name path string true "Variable name"
// @Param request body SetEnvRequest true "Set Env Request"
// @Success 204
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Router /projects/{id}/env/{name} [put]
func (s *Server) setEnv(c echo.Context) error {
	user, _ := currentUser(c)
	id, err := idParam(c, "id")
	if err != nil {
		return projectError(c, err)
	}
	var req SetEnvRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
	}
	if err := s.projects.SetEnv(c.Request().Context(), user, id, c.Param("name"), req.Value); err != nil {
		return projectError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// deleteEnv handles DELETE /api/v1/projects/:id/env/:name
// @Summary Delete an environment variable
// @Tags projects
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path string true "Project ID"
// @Param name path string true "Variable name"
// @Success 204
// @Failure 404 {object} map[string]string
// @Router /projects/{id}/env/{name} [delete]
func (s *Server) deleteEnv(c echo.Context) error {
	user, _ := currentUser(c)
	id, err := idParam(c, "id")
	if err != nil {
		return projectError(c, err)
	}
	if err := s.projects.DeleteEnv(c.Request().Context(), user, id, c.Param("name")); err != nil {
		return projectError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
