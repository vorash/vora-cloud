package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func validSpec() Spec {
	return Spec{
		Project: "acme", Name: "web", Image: "registry.local/acme/web:abc123",
		Port: 8080, Replicas: 2, CPUMillis: 250, MemoryMiB: 256,
		Host: "web.eu.example.com", TLS: true, HealthPath: "/healthz",
		Env: map[string]string{"DATABASE_URL": "postgres://x"},
	}
}

func TestValidateAcceptsAFullSpec(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
}

// Each case is something the cluster would reject or, worse, accept and
// misroute. Failing here means the caller gets a clear error before any
// cluster call.
func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*Spec){
		"uppercase project":         func(s *Spec) { s.Project = "Acme" },
		"project with underscore":   func(s *Spec) { s.Project = "my_proj" },
		"empty name":                func(s *Spec) { s.Name = "" },
		"name over 63 characters":   func(s *Spec) { s.Name = strings.Repeat("a", 64) },
		"no image":                  func(s *Spec) { s.Image = "" },
		"port zero":                 func(s *Spec) { s.Port = 0 },
		"port too large":            func(s *Spec) { s.Port = 70000 },
		"negative replicas":         func(s *Spec) { s.Replicas = -1 },
		"no cpu":                    func(s *Spec) { s.CPUMillis = 0 },
		"no memory":                 func(s *Spec) { s.MemoryMiB = 0 },
		"host with scheme":          func(s *Spec) { s.Host = "https://web.example.com" },
		"host with path":            func(s *Spec) { s.Host = "web.example.com/app" },
		"host uppercase":            func(s *Spec) { s.Host = "Web.example.com" },
		"tls without host":          func(s *Spec) { s.Host = ""; s.TLS = true },
		"health path without slash": func(s *Spec) { s.HealthPath = "healthz" },
		"env name with dash":        func(s *Spec) { s.Env = map[string]string{"BAD-NAME": "x"} },
		"env name starting digit":   func(s *Spec) { s.Env = map[string]string{"1X": "x"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			mutate(&s)
			err := s.Validate()
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("got %v, want ErrInvalidSpec", err)
			}
		})
	}
}

// Zero replicas is a legitimate pause, and no host is a legitimate private
// app: neither may be rejected.
func TestValidateAllowsPausedAndUnexposedApps(t *testing.T) {
	s := validSpec()
	s.Replicas = 0
	s.Host, s.TLS, s.HealthPath = "", false, ""
	if err := s.Validate(); err != nil {
		t.Fatalf("paused, unexposed app rejected: %v", err)
	}
}

type stubDeployer struct{ name string }

func (stubDeployer) Deploy(context.Context, Spec) error                     { return nil }
func (stubDeployer) Status(context.Context, string, string) (Status, error) { return Status{}, nil }
func (stubDeployer) Delete(context.Context, string, string) error           { return nil }
func (stubDeployer) DeleteProject(context.Context, string) error            { return nil }
func (stubDeployer) Health(context.Context) error                           { return nil }

func TestRegistryResolvesConfiguredRegions(t *testing.T) {
	eu := stubDeployer{"eu"}
	r, err := NewRegistry(map[string]Deployer{"eu": eu, "us": stubDeployer{"us"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Deployer("eu")
	if err != nil || got != Deployer(eu) {
		t.Fatalf("Deployer(eu) = %v, %v; want the eu deployer", got, err)
	}
	if regions := strings.Join(r.Regions(), ","); regions != "eu,us" {
		t.Fatalf("Regions() = %q, want eu,us", regions)
	}
}

// A project in an unconfigured region must fail loudly; falling back to
// another region would place the customer's app somewhere they did not pick.
func TestRegistryNeverFallsBackToAnotherRegion(t *testing.T) {
	r, err := NewRegistry(map[string]Deployer{"eu": stubDeployer{"eu"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, region := range []string{"us", "", "EU"} {
		d, err := r.Deployer(region)
		if !errors.Is(err, ErrUnknownRegion) || d != nil {
			t.Fatalf("Deployer(%q) = %v, %v; want ErrUnknownRegion and no deployer", region, d, err)
		}
	}
}

func TestNewRegistryRejectsBadConfiguration(t *testing.T) {
	cases := map[string]map[string]Deployer{
		"no regions":       {},
		"uppercase id":     {"EU": stubDeployer{}},
		"id with a space":  {"eu west": stubDeployer{}},
		"single character": {"e": stubDeployer{}},
		"nil deployer":     {"eu": nil},
	}
	for name, regions := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRegistry(regions); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
