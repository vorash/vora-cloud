# ADR 001 - k3s per region, vora-cloud as the fleet layer

- Status: accepted
- Date: 2026-09-29

## Context

vora-cloud deploys customer apps from GitHub. It will run in several regions
(US, EU, Asia) on different providers, and the client picks the region per
project. The code started on Nomad (`pkg/nomad`, about 350 lines). No cluster
exists today.

## Decision

**1. k3s is the substrate, replacing Nomad.** Reasons: Apache-2.0, where Nomad
1.7 and later is BUSL 1.1 with a hosted-offering clause that matters for a paid
hosted platform; Traefik, containerd and NetworkPolicy come bundled; client-go
has a fake clientset and `envtest` (a real kube-apiserver and etcd without
privileges) for tests that need no cluster; and the Nomad path depended on the
community `containerd-driver` plugin.

**2. One small cluster per region.** k3s supports multicloud, but its docs warn
that latency can hurt cluster health, that embedded etcd is not supported
across locations, and that all servers should sit in one place. etcd wants an
election timeout of at least 10 times the round-trip time, so an
intercontinental cluster detects failures slowly. Each region is one cluster
on one provider's private network.

**3. vora-cloud is the fleet layer.** It owns the API, Postgres, GitHub
webhooks, builds and a client per region, the role vora-api plays for Vora
runners. No Rancher, Fleet, Karmada or Submariner.

**4. The only cross-region link is vora-cloud to each region's API** over
WireGuard or Tailscale. There is no cross-region pod network and no BGP or
anycast: a project's DNS record points at the ingress of the region the client
chose.

## Consequences

- Operations grow linearly with regions (upgrades, certificates, backups,
  monitoring). Each region is built from one Ansible inventory to keep that
  bounded.
- A single k3s server per region means losing it stops deploys and healing
  there, not the running apps. HA (three servers) is a later, per-region choice.
- A partition between vora-cloud and a region only blocks new deploys; the
  region's apps and cluster are unaffected.
- gVisor is not auto-detected by k3s. Hosting untrusted customer code later
  needs a containerd config template and a RuntimeClass.
- Lambda-like jobs are not a k3s feature; they run through the Vora API.
