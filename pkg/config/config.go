package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/vorash/vora-cloud/pkg/utils"
)

type Config struct {
	GRPCPort string
	HttpPort string
	// Regions is "<region>=<kubeconfig path>,..."; see ParseRegions.
	Regions      string
	IngressClass string
	DBConnection string
	// DBMaxConns and DBMinConns size the service's connection pool; see ParsePoolSize.
	DBMaxConns   string
	DBMinConns   string
	BootstrapKey string
	// PublicURL is the site origin, e.g. https://cloud.vora.sh; the dashboard
	// accepts writes only from it.
	PublicURL string
	// AllowRegistration opens POST /auth/register to anyone. Off by default:
	// users are created with an admin API key.
	AllowRegistration bool
	JWTSecret         string
	// SecretsKey is a base64 32 byte key that encrypts project env vars.
	SecretsKey string
	// AppsDomain is the base of app hosts: <slug>.<region>.<AppsDomain>.
	AppsDomain string
	// GitHubWebhookSecret is the GitHub App's webhook secret. Without it the
	// webhook endpoint is not served.
	GitHubWebhookSecret string
}

func LoadConfig() *Config {
	return &Config{
		GRPCPort:          utils.GetEnv("GRPC_PORT", "50051"),
		HttpPort:          utils.GetEnv("HTTP_PORT", "5000"),
		Regions:           utils.GetEnv("REGIONS", ""),
		IngressClass:      utils.GetEnv("INGRESS_CLASS", "traefik"),
		DBConnection:      utils.GetEnv("DATABASE_URL", "postgres://postgres@localhost:5432/control-plane"),
		DBMaxConns:        utils.GetEnv("DB_MAX_CONNS", "25"),
		DBMinConns:        utils.GetEnv("DB_MIN_CONNS", "5"),
		BootstrapKey:      utils.GetEnv("BOOTSTRAP_KEY", ""),
		PublicURL:         utils.GetEnv("PUBLIC_URL", ""),
		AllowRegistration: utils.GetEnv("ALLOW_REGISTRATION", "") == "true",
		JWTSecret:         utils.GetEnv("JWT_SECRET", ""),
		SecretsKey:        utils.GetEnv("SECRETS_KEY", ""),
		AppsDomain:        utils.GetEnv("APPS_DOMAIN", ""),

		GitHubWebhookSecret: utils.GetEnv("GITHUB_WEBHOOK_SECRET", ""),
	}
}

// ParseRegions reads "eu=/etc/vora-cloud/eu.kubeconfig,us=/etc/vora-cloud/us.kubeconfig"
// into region id -> kubeconfig path. It rejects an empty list, a malformed
// pair and a repeated region, because a region silently pointing at the wrong
// cluster would deploy customers' apps to the wrong place.
func ParseRegions(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("REGIONS is required, e.g. eu=/path/to/kubeconfig")
	}
	regions := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		id, path, ok := strings.Cut(strings.TrimSpace(pair), "=")
		id, path = strings.TrimSpace(id), strings.TrimSpace(path)
		if !ok || id == "" || path == "" {
			return nil, fmt.Errorf("REGIONS entry %q must look like <region>=<kubeconfig path>", pair)
		}
		if _, dup := regions[id]; dup {
			return nil, fmt.Errorf("REGIONS lists %q twice", id)
		}
		regions[id] = path
	}
	return regions, nil
}

// ParsePoolSize reads DB_MAX_CONNS and DB_MIN_CONNS. Behind Supabase's session
// pooler every connection holds a backend connection, and the pooler's pool
// size is a plan limit, so a pool larger than it fails at connect time with
// "max clients reached". It rejects values that cannot work.
func ParsePoolSize(rawMax, rawMin string) (maxConns, minConns int32, err error) {
	maxN, err := strconv.ParseInt(strings.TrimSpace(rawMax), 10, 32)
	if err != nil || maxN < 1 {
		return 0, 0, fmt.Errorf("DB_MAX_CONNS must be a whole number of at least 1, got %q", rawMax)
	}
	minN, err := strconv.ParseInt(strings.TrimSpace(rawMin), 10, 32)
	if err != nil || minN < 0 {
		return 0, 0, fmt.Errorf("DB_MIN_CONNS must be a whole number of at least 0, got %q", rawMin)
	}
	if minN > maxN {
		return 0, 0, fmt.Errorf("DB_MIN_CONNS (%d) is larger than DB_MAX_CONNS (%d)", minN, maxN)
	}
	return int32(maxN), int32(minN), nil
}
