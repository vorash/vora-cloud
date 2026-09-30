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
