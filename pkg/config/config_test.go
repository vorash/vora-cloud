package config

import "testing"

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
