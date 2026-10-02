package api

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/kubernetes/fake"

	pb "github.com/vorash/vora-cloud/api/proto"
	"github.com/vorash/vora-cloud/pkg/deploy"
	"github.com/vorash/vora-cloud/pkg/deploy/k3s"
	"github.com/vorash/vora-cloud/pkg/project"
	"github.com/vorash/vora-cloud/pkg/project/projecttest"
	"github.com/vorash/vora-cloud/pkg/secrets"
)

func newProjectsService(t *testing.T) *ProjectsService {
	t.Helper()
	reg, err := deploy.NewRegistry(map[string]deploy.Deployer{"eu": k3s.New(fake.NewClientset(), k3s.Options{})})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return NewProjectsService(project.NewService(projecttest.New(), reg, box, "cloud.example.com"))
}

func as(user uuid.UUID) context.Context {
	return context.WithValue(context.Background(), userContextKey{}, user)
}

func wantCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Errorf("%s: %v, want %v (%v)", what, got, want, err)
	}
}

func TestProjectsServiceRefusesACallWithoutAUser(t *testing.T) {
	s := newProjectsService(t)
	_, err := s.ListProjects(context.Background(), &pb.ListProjectsRequest{})
	wantCode(t, "no user in the context", err, codes.Unauthenticated)
}

func TestProjectsServiceHidesProjectsFromOtherUsers(t *testing.T) {
	s := newProjectsService(t)
	bob := uuid.New()
	p, err := s.CreateProject(as(alice), &pb.CreateProjectRequest{Name: "Web", Slug: "web", Region: "eu"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.DeployImage(as(alice), &pb.DeployImageRequest{ProjectId: p.Id, Image: "reg/web:1"})
	if err != nil || d.Status != "deploying" || d.Url != "https://web.eu.cloud.example.com" {
		t.Fatalf("deploy: %+v, %v", d, err)
	}

	_, err = s.GetProject(as(bob), &pb.GetProjectRequest{Id: p.Id})
	wantCode(t, "get", err, codes.NotFound)
	_, err = s.DeleteProject(as(bob), &pb.DeleteProjectRequest{Id: p.Id})
	wantCode(t, "delete", err, codes.NotFound)
	_, err = s.ListDeployments(as(bob), &pb.ListDeploymentsRequest{ProjectId: p.Id})
	wantCode(t, "list deployments", err, codes.NotFound)
	_, err = s.GetDeployment(as(bob), &pb.GetDeploymentRequest{Id: d.Id})
	wantCode(t, "get deployment", err, codes.NotFound)
	_, err = s.DeployImage(as(bob), &pb.DeployImageRequest{ProjectId: p.Id, Image: "evil:1"})
	wantCode(t, "deploy", err, codes.NotFound)
	_, err = s.Redeploy(as(bob), &pb.RedeployRequest{ProjectId: p.Id})
	wantCode(t, "redeploy", err, codes.NotFound)
	_, err = s.Rollback(as(bob), &pb.RollbackRequest{ProjectId: p.Id, DeploymentId: d.Id})
	wantCode(t, "rollback", err, codes.NotFound)
	_, err = s.GetProject(as(bob), &pb.GetProjectRequest{Id: "not-a-uuid"})
	wantCode(t, "malformed id", err, codes.NotFound)

	if list, _ := s.ListProjects(as(bob), &pb.ListProjectsRequest{}); len(list.Projects) != 0 {
		t.Errorf("bob lists %d projects", len(list.Projects))
	}
	if _, err := s.GetProject(as(alice), &pb.GetProjectRequest{Id: p.Id}); err != nil {
		t.Errorf("the owner lost the project: %v", err)
	}
}

func TestProjectsServiceMapsErrorsToCodes(t *testing.T) {
	s := newProjectsService(t)
	if _, err := s.CreateProject(as(alice), &pb.CreateProjectRequest{Name: "Web", Slug: "web", Region: "eu"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateProject(as(uuid.New()), &pb.CreateProjectRequest{Name: "Other", Slug: "web", Region: "eu"})
	wantCode(t, "slug of another user", err, codes.AlreadyExists)
	_, err = s.CreateProject(as(alice), &pb.CreateProjectRequest{Name: "X", Slug: "Bad_Slug", Region: "eu"})
	wantCode(t, "bad slug", err, codes.InvalidArgument)
	_, err = s.Rollback(as(alice), &pb.RollbackRequest{ProjectId: uuid.NewString(), DeploymentId: "nope"})
	wantCode(t, "bad deployment id", err, codes.InvalidArgument)
}

func TestProjectsServiceListsDeploymentsInPages(t *testing.T) {
	s := newProjectsService(t)
	p, err := s.CreateProject(as(alice), &pb.CreateProjectRequest{Name: "Web", Slug: "web", Region: "eu"})
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range []string{"reg/web:1", "reg/web:2", "reg/web:3"} {
		if _, err := s.DeployImage(as(alice), &pb.DeployImageRequest{ProjectId: p.Id, Image: img}); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.ListDeployments(as(alice), &pb.ListDeploymentsRequest{ProjectId: p.Id})
	if err != nil || len(all.Deployments) != 3 || all.NextCursor != "" {
		t.Fatalf("no paging asked: %+v, %v; want everything and no cursor", all, err)
	}
	first, err := s.ListDeployments(as(alice), &pb.ListDeploymentsRequest{ProjectId: p.Id, Limit: 2})
	if err != nil || len(first.Deployments) != 2 || first.Deployments[0].Image != "reg/web:3" || first.NextCursor == "" {
		t.Fatalf("first page: %+v, %v", first, err)
	}
	older, err := s.ListDeployments(as(alice), &pb.ListDeploymentsRequest{ProjectId: p.Id, Limit: 2, Before: first.NextCursor})
	if err != nil || len(older.Deployments) != 1 || older.Deployments[0].Image != "reg/web:1" || older.NextCursor != "" {
		t.Errorf("older page: %+v, %v", older, err)
	}
	_, err = s.ListDeployments(as(alice), &pb.ListDeploymentsRequest{ProjectId: p.Id, Before: "nope"})
	wantCode(t, "a cursor that is not one", err, codes.InvalidArgument)
}
