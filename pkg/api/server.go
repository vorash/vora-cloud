package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	pb "github.com/vorash/vora-cloud/api/proto"
	"github.com/vorash/vora-cloud/pkg/deploy"
)

// defaultContainerPort is what apps listen on; the request has no port field.
const defaultContainerPort = 80

// ApplicationService serves the gRPC API on top of the per-region deployers.
//
// Until projects exist (M3) an application is its own project, and its
// deployment id is "<region>/<name>" because status and delete requests carry
// no region.
type ApplicationService struct {
	pb.UnimplementedControlPlaneServer
	regions *deploy.Registry
}

func NewApplicationService(regions *deploy.Registry) *ApplicationService {
	return &ApplicationService{regions: regions}
}

// DeployApplication deploys an application to the requested region.
func (s *ApplicationService) DeployApplication(ctx context.Context, req *pb.DeployRequest) (*pb.DeployResponse, error) {
	fail := func(err error) (*pb.DeployResponse, error) {
		return &pb.DeployResponse{Status: "FAILED", Message: fmt.Sprintf("Failed to deploy application: %v", err)}, nil
	}

	deployer, err := s.regions.Deployer(req.Region)
	if err != nil {
		return fail(err)
	}
	spec, err := specFromRequest(req)
	if err != nil {
		return fail(err)
	}
	if err := deployer.Deploy(ctx, spec); err != nil {
		return fail(err)
	}

	return &pb.DeployResponse{
		DeploymentId: deploymentID(req.Region, req.Name),
		Status:       "SUBMITTED",
		Message:      "Application deployment submitted successfully",
	}, nil
}

// unsupportedTraefik lists Traefik options that only made sense as Nomad
// service tags. They are rejected instead of silently ignored.
func unsupportedTraefik(t *pb.TraefikConfig) []string {
	var set []string
	add := func(name string, isSet bool) {
		if isSet {
			set = append(set, name)
		}
	}
	add("entrypoint", t.Entrypoint != "")
	add("ssl_host", t.SslHost != "")
	add("cert_resolver", t.CertResolver != "")
	add("health_check_interval", t.HealthCheckInterval != "")
	add("path_prefix", t.PathPrefix != "")
	add("middlewares", len(t.Middlewares) > 0)
	add("custom_labels", len(t.CustomLabels) > 0)
	return set
}

// specFromRequest maps the wire request to a deploy.Spec. network_mode is
// ignored: pods use the cluster network, and the CLI sends "host" by default.
func specFromRequest(req *pb.DeployRequest) (deploy.Spec, error) {
	spec := deploy.Spec{
		Project:   req.Name,
		Name:      req.Name,
		Image:     req.Image,
		Port:      defaultContainerPort,
		Replicas:  req.Replicas,
		Env:       req.Labels,
		CPUMillis: int64(req.Cpu * 1000),
		MemoryMiB: req.Memory,
	}
	if t := req.Traefik; t != nil {
		if bad := unsupportedTraefik(t); len(bad) > 0 {
			return deploy.Spec{}, fmt.Errorf("traefik options not supported on k3s: %s", strings.Join(bad, ", "))
		}
		if t.Enable {
			spec.Host = t.Host
			spec.TLS = t.EnableSsl
			spec.HealthPath = t.HealthCheckPath
		}
	}
	if err := spec.Validate(); err != nil {
		return deploy.Spec{}, err
	}
	return spec, nil
}

func deploymentID(region, name string) string { return region + "/" + name }

// parseDeploymentID splits "<region>/<name>". Anything else is rejected so a
// stale id from the Nomad era cannot address the wrong app.
func parseDeploymentID(id string) (region, name string, err error) {
	region, name, ok := strings.Cut(id, "/")
	if !ok || region == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("deployment id %q must look like <region>/<name>", id)
	}
	return region, name, nil
}

// deployerFor resolves the region inside a deployment id.
func (s *ApplicationService) deployerFor(id string) (deploy.Deployer, string, error) {
	region, name, err := parseDeploymentID(id)
	if err != nil {
		return nil, "", err
	}
	deployer, err := s.regions.Deployer(region)
	if err != nil {
		return nil, "", err
	}
	return deployer, name, nil
}

// DeleteApplication deletes an application and everything it created.
func (s *ApplicationService) DeleteApplication(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	fail := func(err error) (*pb.DeleteResponse, error) {
		return &pb.DeleteResponse{Success: false, Message: fmt.Sprintf("Failed to delete application: %v", err)}, nil
	}

	deployer, name, err := s.deployerFor(req.DeploymentId)
	if err != nil {
		return fail(err)
	}
	if err := deployer.DeleteProject(ctx, name); err != nil {
		return fail(err)
	}
	return &pb.DeleteResponse{Success: true, Message: "Application deleted successfully"}, nil
}

// GetApplicationStatus retrieves the status of an application.
func (s *ApplicationService) GetApplicationStatus(ctx context.Context, req *pb.StatusRequest) (*pb.StatusResponse, error) {
	fail := func(err error) (*pb.StatusResponse, error) {
		return &pb.StatusResponse{
			DeploymentId: req.DeploymentId,
			Message:      fmt.Sprintf("Failed to get application status: %v", err),
		}, nil
	}

	deployer, name, err := s.deployerFor(req.DeploymentId)
	if err != nil {
		return fail(err)
	}
	st, err := deployer.Status(ctx, name, name)
	if err != nil {
		return fail(err)
	}

	msg := "Application status retrieved successfully"
	if st.Message != "" {
		msg = st.Message
	}
	return &pb.StatusResponse{
		DeploymentId:     req.DeploymentId,
		JobStatus:        string(st.State),
		JobType:          "service",
		DesiredInstances: st.Desired,
		RunningInstances: st.Ready,
		Message:          msg,
	}, nil
}

// HealthCheck reports NOT_SERVING when any region's cluster is unreachable.
func (s *ApplicationService) HealthCheck(ctx context.Context, req *pb.HealthCheckRequest) (*pb.HealthCheckResponse, error) {
	status := pb.HealthStatus_SERVING
	message := "Service is healthy"

	var down []string
	for _, region := range s.regions.Regions() {
		deployer, err := s.regions.Deployer(region)
		if err == nil {
			err = deployer.Health(ctx)
		}
		if err != nil {
			down = append(down, fmt.Sprintf("%s: %v", region, err))
		}
	}
	if len(down) > 0 {
		status = pb.HealthStatus_NOT_SERVING
		message = "Unhealthy regions: " + strings.Join(down, "; ")
	}

	return &pb.HealthCheckResponse{
		Status:    status,
		Message:   message,
		Timestamp: time.Now().Unix(),
	}, nil
}
