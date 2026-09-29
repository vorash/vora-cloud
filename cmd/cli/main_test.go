package main

import "testing"

func TestTraefikForIsNilWithoutAHost(t *testing.T) {
	if got := traefikFor("", true); got != nil {
		t.Errorf("got %+v, want nil so the app is not exposed", got)
	}
}

// The server rejects Nomad-only Traefik options, so a CLI that sent them
// would fail every deploy that uses -host.
func TestTraefikForSendsOnlyOptionsTheServerSupports(t *testing.T) {
	got := traefikFor("web.eu.example.com", true)
	if !got.Enable || got.Host != "web.eu.example.com" || !got.EnableSsl || got.HealthCheckPath != "/" {
		t.Errorf("config = %+v", got)
	}
	if got.Entrypoint != "" || got.SslHost != "" || got.CertResolver != "" || got.HealthCheckInterval != "" ||
		got.PathPrefix != "" || len(got.Middlewares) > 0 || len(got.CustomLabels) > 0 {
		t.Errorf("config sets options the server rejects: %+v", got)
	}
}
