# ADR 002 - Region model

- Status: accepted
- Date: 2026-09-29

## Context

ADR 001 makes each region an independent cluster. Only region `eu` exists for
now, but the API and data must not need a redesign when a second region is
added.

## Decision

**1. A region is a short lowercase id** (`eu`, `us`, `asia`) that names a
cluster and its ingress domain (`*.eu.<domain>`).

**2. `region` is a column on `projects` and `deployments`.** A project has one
region; a deployment records the region it ran in, so history stays correct if
a project moves.

**3. The deploy interface takes a region.** A registry resolves the region to a
per-region client from configuration. Only `eu` is configured to begin with;
an unknown region is an error, never a default.

**4. Region 1 is `eu`, on a single VM** running vora-cloud, Postgres, the
registry and k3s. The global plane (vora-cloud and Postgres) moves off it when
region 2 arrives.

## Consequences

- Adding a region is configuration and infrastructure, not a schema change.
- The registry topology (a central registry with per-region pull-through
  caches) is decided when the second region is planned.
- Vora's placement has no region concept today, so Vora-backed features need
  their own region handling.
