# vora-cloud

A Go service for deploying containerized applications to Kubernetes (k3s): connect GitHub, build, deploy. It exposes a gRPC API, a REST API with Swagger UI, and a command-line client.

> **Status:** under construction. Deploying an image works; GitHub, builds, projects and the dashboard are still on the roadmap in [#2](https://github.com/vorash/vora-cloud/issues/2). Design decisions are in [docs/adr](docs/adr).

## How it works

Each region is one small k3s cluster. vora-cloud sits above the clusters, holds one deployer per region, and sends every request to the region the caller named ([ADR 001](docs/adr/001-k3s-per-region.md), [ADR 002](docs/adr/002-region-model.md)).

```text
  CLI · REST · gRPC clients
            │
       vora-cloud ── Postgres (users, API keys)
            │  region id -> kubeconfig
    ┌───────┴────────┐
    ▼                ▼
  eu cluster      us cluster      (only regions listed in REGIONS exist)
  k3s + Traefik
```

For each app, a deployment creates a namespace per project, a Secret with the environment, a Deployment (rolling update that never removes a serving pod first, readiness probe, requests equal to limits), a Service, and, when a host is set, a Traefik Ingress. Deploying the same app twice changes nothing.

## Requirements

- Go 1.26 or later
- A k3s cluster per region and a kubeconfig for each, with Traefik as the ingress class
- PostgreSQL, for users and API keys
- `protoc` with `protoc-gen-go` and `protoc-gen-go-grpc` to regenerate the API code (`make install-tools`)
- The [`migrate`](https://github.com/golang-migrate/migrate) CLI for `make migrate-up`

## Run it

```bash
git clone git@github.com:vorash/vora-cloud.git
cd vora-cloud
make build

cp .env.example .env    # then edit it
```

The controller reads its configuration from the environment:

| Variable | Meaning |
|----------|---------|
| `REGIONS` | Required. `<region>=<kubeconfig path>`, comma separated, e.g. `eu=/etc/vora-cloud/eu.kubeconfig`. The server refuses to start if it is empty or malformed |
| `INGRESS_CLASS` | Ingress class for apps (default `traefik`) |
| `DATABASE_URL` | PostgreSQL connection string |
| `JWT_SECRET` | Required, at least 32 characters |
| `BOOTSTRAP_KEY` | Optional. Creates an admin API key on first run |
| `GRPC_PORT`, `HTTP_PORT` | Default `50051` and `5000` |

```bash
DATABASE_URL=postgres://... make migrate-up
set -a; . ./.env; set +a
./bin/controller
```

Deploy an app to the `eu` region and check it:

```bash
./bin/cli -action=deploy -region=eu -name=whoami -image=traefik/whoami:latest \
  -host=whoami.eu.example.com -ssl
./bin/cli -action=status -region=eu -name=whoami
./bin/cli -action=delete -region=eu -name=whoami
```

## API

### Deployment ids

A deployment id is `<region>/<name>`, for example `eu/whoami`. Deploy returns it, and status and delete take it, because those requests carry no region. Ids without a region are rejected. In REST paths send the slash as `%2F`: `/api/v1/applications/eu%2Fwhoami/status`.

### gRPC

The service is `ControlPlane` in [`api/proto/controlplane.proto`](api/proto/controlplane.proto) (`DeployApplication`, `DeleteApplication`, `GetApplicationStatus`, `HealthCheck`; `GetApplicationLogs` is not implemented yet).

`DeployRequest` fields:

| Field | Notes |
|-------|-------|
| `name` | DNS label (lowercase letters, digits, `-`). Until projects exist, an app is its own project |
| `image` | Required |
| `replicas` | `0` pauses the app |
| `cpu`, `memory` | Cores and MB, both required. They are the request and the limit |
| `region` | Required. An empty or unknown region fails; there is no default |
| `labels` | Become the app's environment variables |
| `traefik` | See below |
| `network_mode` | Ignored: pods use the cluster network |

The container port is fixed at 80 for now.

Supported `TraefikConfig` fields: `enable`, `host`, `enable_ssl` and `health_check_path`. Setting any of `entrypoint`, `ssl_host`, `cert_resolver`, `health_check_interval`, `path_prefix`, `middlewares` or `custom_labels` is rejected, because they only made sense as Nomad service tags.

`StatusResponse.job_status` is one of `pending`, `rolling_out`, `ready`, `failed` or `not_found`. A rollout that makes no progress for five minutes is `failed`.

### REST

Served on `HTTP_PORT` under `/api/v1`, with Swagger UI at `/swagger/index.html`.

| Route | Purpose |
|-------|---------|
| `POST /applications` | Deploy |
| `DELETE /applications/:id` | Delete |
| `GET /applications/:id/status` | Status |
| `/auth/*` | Register, log in, refresh, log out |
| `/user/keys`, `/admin/keys` | API keys |

## CLI

```bash
./bin/cli -action=<deploy|delete|status|health> [flags]
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-server` | `localhost:50051` | gRPC address |
| `-region` | none | Required. Region id, e.g. `eu` |
| `-name` | none | App name |
| `-image` | none | Container image |
| `-replicas` | `1` | Instances |
| `-cpu` | `0.1` | CPU cores |
| `-memory` | `128` | Memory in MB |
| `-host` | none | Public hostname (enables the ingress) |
| `-ssl` | `false` | Serve the host over HTTPS |
| `-delete-id` | none | Deployment id to delete, instead of `-region` and `-name` |
| `-network` | `host` | Accepted for compatibility; ignored |

## Development

```bash
make check          # the merge gate: gofmt, go vet, golangci-lint (if installed), tests, build
make test-envtest    # run the deployer against a real kube-apiserver and etcd (downloads them once)
make test-migrations # run the migrations on throwaway PostgreSQL clusters (needs PG_BIN=<dir with initdb> and the migrate CLI)
make proto          # regenerate the gRPC code after editing controlplane.proto
make install-tools  # protoc plugins and golangci-lint
make migrate-up     # apply migrations (needs DATABASE_URL and the migrate CLI)
```

GitHub Actions is not available for this repository, so `make check` is the merge gate and must pass locally before a change is merged. Use `devbox shell` for a ready environment (Go 1.26).

Code layout:

| Path | Contents |
|------|----------|
| `pkg/deploy` | The region-agnostic `Deployer` interface, `Spec` validation, and the region `Registry` |
| `pkg/deploy/k3s` | The Kubernetes implementation (server-side apply with client-go) |
| `pkg/api` | The gRPC service, on top of the registry |
| `pkg/rest` | REST handlers and routes |
| `pkg/auth`, `pkg/database` | Users, API keys and JWT, on PostgreSQL |
| `cmd/controller`, `cmd/cli` | The server and the command-line client |
| `docs/adr` | Architecture decision records |
