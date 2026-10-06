package config

import (
	"strings"
	"testing"
)

func TestParseRegions(t *testing.T) {
	got, err := ParseRegions(" eu=/etc/a.yaml , us=/etc/b.yaml ")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["eu"] != "/etc/a.yaml" || got["us"] != "/etc/b.yaml" {
		t.Errorf("got %v", got)
	}
}

// Each of these would leave a region unconfigured or pointing somewhere
// unintended, so startup must stop instead of guessing.
func TestParseRegionsRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":           "",
		"blank":           "   ",
		"no equals":       "eu",
		"no path":         "eu=",
		"no region":       "=/etc/a.yaml",
		"trailing comma":  "eu=/etc/a.yaml,",
		"repeated region": "eu=/etc/a.yaml,eu=/etc/b.yaml",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := ParseRegions(raw); err == nil {
				t.Errorf("ParseRegions(%q) = %v, want an error", raw, got)
			}
		})
	}
}

func TestParsePoolSize(t *testing.T) {
	max, min, err := ParsePoolSize(" 10 ", "2")
	if err != nil || max != 10 || min != 2 {
		t.Errorf("got %d %d %v", max, min, err)
	}
	if _, _, err := ParsePoolSize("25", "0"); err != nil {
		t.Errorf("a pool that opens no idle connections is valid: %v", err)
	}
}

// A pool that cannot work must stop startup, not surface later as failed
// queries or as "max clients reached" from the pooler.
func TestParsePoolSizeRejects(t *testing.T) {
	for name, c := range map[string][2]string{
		"zero max":       {"0", "0"},
		"negative max":   {"-1", "0"},
		"not a number":   {"many", "5"},
		"empty max":      {"", "5"},
		"negative min":   {"10", "-1"},
		"min above max":  {"5", "10"},
		"min not number": {"10", "x"},
	} {
		if _, _, err := ParsePoolSize(c[0], c[1]); err == nil {
			t.Errorf("%s: accepted %v", name, c)
		}
	}
}

// The connect flow needs all four settings. With one missing it would start
// and then fail on the first user, so startup must stop instead.
func TestGitHubAppIsAllOrNothing(t *testing.T) {
	full := Config{GitHubAppSlug: "app", GitHubClientID: "id", GitHubClientSecret: "secret", GitHubPrivateKeyFile: "/key.pem"}
	if on, err := full.GitHubApp(); !on || err != nil {
		t.Errorf("all set: %v, %v", on, err)
	}
	if on, err := (&Config{}).GitHubApp(); on || err != nil {
		t.Errorf("none set: %v, %v", on, err)
	}
	for name, mutate := range map[string]func(*Config){
		"no slug":   func(c *Config) { c.GitHubAppSlug = "" },
		"no id":     func(c *Config) { c.GitHubClientID = "" },
		"no secret": func(c *Config) { c.GitHubClientSecret = "" },
		"no key":    func(c *Config) { c.GitHubPrivateKeyFile = "" },
	} {
		t.Run(name, func(t *testing.T) {
			c := full
			mutate(&c)
			if on, err := c.GitHubApp(); on || err == nil {
				t.Errorf("got %v, %v; want an error", on, err)
			}
		})
	}
}

// A build needs the GitHub App to read the repository, a registry to push to, and
// a BuildKit image that cannot change under it. A partial set would start and then
// fail on the first push, so startup must stop instead.
func TestBuildsAreAllOrNothingAndNeedTheAppAndAPinnedImage(t *testing.T) {
	app := Config{GitHubAppSlug: "app", GitHubClientID: "id", GitHubClientSecret: "secret", GitHubPrivateKeyFile: "/key.pem"}
	full := app
	full.BuildRegistryPush, full.BuildRegistryPull, full.BuildRegistryAPI = "registry:5000", "localhost:5000", "http://127.0.0.1:5000"
	full.BuildKitImage = "moby/buildkit:v0.33.1-rootless@sha256:" + strings.Repeat("a", 64)

	if on, err := full.Builds(); !on || err != nil {
		t.Errorf("all set: %v, %v", on, err)
	}
	if on, err := (&Config{}).Builds(); on || err != nil {
		t.Errorf("none set: %v, %v", on, err)
	}
	for name, mutate := range map[string]func(*Config){
		"no push registry":    func(c *Config) { c.BuildRegistryPush = "" },
		"no pull registry":    func(c *Config) { c.BuildRegistryPull = "" },
		"no registry API":     func(c *Config) { c.BuildRegistryAPI = "" },
		"no image":            func(c *Config) { c.BuildKitImage = "" },
		"a tag, not a digest": func(c *Config) { c.BuildKitImage = "moby/buildkit:rootless" },
		"no GitHub App":       func(c *Config) { c.GitHubClientSecret = "" },
		"half a GitHub App":   func(c *Config) { c.GitHubAppSlug = "" },
	} {
		t.Run(name, func(t *testing.T) {
			c := full
			mutate(&c)
			if on, err := c.Builds(); on || err == nil {
				t.Errorf("got %v, %v; want an error", on, err)
			}
		})
	}
	// Builds without the App, with none of the App's settings at all.
	noApp := full
	noApp.GitHubAppSlug, noApp.GitHubClientID, noApp.GitHubClientSecret, noApp.GitHubPrivateKeyFile = "", "", "", ""
	if on, err := noApp.Builds(); on || err == nil {
		t.Errorf("builds without the App: %v, %v", on, err)
	}
}
