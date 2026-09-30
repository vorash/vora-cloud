package api

import (
	"context"
	"errors"
	"log"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/vorash/vora-cloud/api/proto"
	"github.com/vorash/vora-cloud/pkg/project"
)

// ProjectsService serves the Projects gRPC service. Callers reach it through
// AuthInterceptor, which puts the user in the context; a call without a user
// is refused here as well, so a mis-wired server does not become open.
type ProjectsService struct {
	pb.UnimplementedProjectsServer
	projects *project.Service
}

func NewProjectsService(projects *project.Service) *ProjectsService {
	return &ProjectsService{projects: projects}
}

func caller(ctx context.Context) (uuid.UUID, error) {
	id, ok := UserFromContext(ctx)
	if !ok {
		return uuid.UUID{}, status.Error(codes.Unauthenticated, "no authenticated user")
	}
	return id, nil
}

// parseID reports a malformed id as missing, the same as an id of someone else.
func parseID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, project.ErrNotFound
	}
	return id, nil
}

// statusError maps a service error to a gRPC status. Unexpected errors are
// logged and answered generically.
func statusError(method string, err error) error {
	switch {
	case errors.Is(err, project.ErrNotFound):
		return status.Error(codes.NotFound, "not found")
	case errors.Is(err, project.ErrSlugTaken):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, project.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		log.Printf("%s: %v", method, err)
		return status.Error(codes.Internal, "internal error")
	}
}

func projectMessage(p *project.Project) *pb.Project {
	return &pb.Project{
		Id: p.ID.String(), Name: p.Name, Slug: p.Slug, Repo: p.Repo, DefaultBranch: p.DefaultBranch,
		Region: p.Region, Port: p.Port, Replicas: p.Replicas, CpuMillis: p.CPUMillis,
		MemoryMib: p.MemoryMiB, HealthPath: p.HealthPath, CreatedAt: p.CreatedAt.Unix(),
	}
}

func deploymentMessage(d *project.Deployment) *pb.Deployment {
	m := &pb.Deployment{
		Id: d.ID.String(), ProjectId: d.ProjectID.String(), CommitSha: d.CommitSHA, Ref: d.Ref,
		Status: string(d.Status), Image: d.Image, Region: d.Region, Url: d.URL, Error: d.Error,
		CreatedAt: d.CreatedAt.Unix(),
	}
	if d.StartedAt != nil {
		m.StartedAt = d.StartedAt.Unix()
	}
	if d.FinishedAt != nil {
		m.FinishedAt = d.FinishedAt.Unix()
	}
	return m
}

func (s *ProjectsService) CreateProject(ctx context.Context, req *pb.CreateProjectRequest) (*pb.Project, error) {
	user, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.projects.CreateProject(ctx, user, project.CreateInput{
		Name: req.Name, Slug: req.Slug, Region: req.Region, Repo: req.Repo, DefaultBranch: req.DefaultBranch,
		Port: req.Port, Replicas: req.Replicas, CPUMillis: req.CpuMillis, MemoryMiB: req.MemoryMib,
		HealthPath: req.HealthPath,
	})
	if err != nil {
		return nil, statusError("CreateProject", err)
	}
	return projectMessage(p), nil
}

func (s *ProjectsService) ListProjects(ctx context.Context, _ *pb.ListProjectsRequest) (*pb.ListProjectsResponse, error) {
	user, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.projects.ListProjects(ctx, user)
	if err != nil {
		return nil, statusError("ListProjects", err)
	}
	out := &pb.ListProjectsResponse{}
	for i := range list {
		out.Projects = append(out.Projects, projectMessage(&list[i]))
	}
	return out, nil
}

func (s *ProjectsService) GetProject(ctx context.Context, req *pb.GetProjectRequest) (*pb.Project, error) {
	user, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(req.Id)
	if err != nil {
		return nil, statusError("GetProject", err)
	}
	p, err := s.projects.GetProject(ctx, user, id)
	if err != nil {
		return nil, statusError("GetProject", err)
	}
	return projectMessage(p), nil
}

func (s *ProjectsService) DeleteProject(ctx context.Context, req *pb.DeleteProjectRequest) (*pb.DeleteProjectResponse, error) {
	user, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(req.Id)
	if err != nil {
		return nil, statusError("DeleteProject", err)
	}
	if err := s.projects.DeleteProject(ctx, user, id); err != nil {
		return nil, statusError("DeleteProject", err)
	}
	return &pb.DeleteProjectResponse{}, nil
}

func (s *ProjectsService) ListDeployments(ctx context.Context, req *pb.ListDeploymentsRequest) (*pb.ListDeploymentsResponse, error) {
	user, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(req.ProjectId)
	if err != nil {
		return nil, statusError("ListDeployments", err)
	}
	deps, err := s.projects.ListDeployments(ctx, user, id)
	if err != nil {
		return nil, statusError("ListDeployments", err)
	}
	out := &pb.ListDeploymentsResponse{}
	for i := range deps {
		out.Deployments = append(out.Deployments, deploymentMessage(&deps[i]))
	}
	return out, nil
}

func (s *ProjectsService) GetDeployment(ctx context.Context, req *pb.GetDeploymentRequest) (*pb.Deployment, error) {
	user, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(req.Id)
	if err != nil {
		return nil, statusError("GetDeployment", err)
	}
	d, err := s.projects.GetDeployment(ctx, user, id)
	if err != nil {
		return nil, statusError("GetDeployment", err)
	}
	return deploymentMessage(d), nil
}

func (s *ProjectsService) DeployImage(ctx context.Context, req *pb.DeployImageRequest) (*pb.Deployment, error) {
	user, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(req.ProjectId)
	if err != nil {
		return nil, statusError("DeployImage", err)
	}
	d, err := s.projects.Deploy(ctx, user, id, project.DeployInput{Image: req.Image, CommitSHA: req.CommitSha, Ref: req.Ref})
	if err != nil {
		return nil, statusError("DeployImage", err)
	}
	return deploymentMessage(d), nil
}

func (s *ProjectsService) Redeploy(ctx context.Context, req *pb.RedeployRequest) (*pb.Deployment, error) {
	user, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(req.ProjectId)
	if err != nil {
		return nil, statusError("Redeploy", err)
	}
	d, err := s.projects.Redeploy(ctx, user, id)
	if err != nil {
		return nil, statusError("Redeploy", err)
	}
	return deploymentMessage(d), nil
}

func (s *ProjectsService) Rollback(ctx context.Context, req *pb.RollbackRequest) (*pb.Deployment, error) {
	user, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID(req.ProjectId)
	if err != nil {
		return nil, statusError("Rollback", err)
	}
	target, err := uuid.Parse(req.DeploymentId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "deployment_id must be a deployment id")
	}
	d, err := s.projects.Rollback(ctx, user, id, target)
	if err != nil {
		return nil, statusError("Rollback", err)
	}
	return deploymentMessage(d), nil
}
