# ADR 006 - Build pipeline

- Status: accepted
- Date: 2026-10-06
- Amended: 2026-10-06 (what the orchestration settled, and what was verified live)

## Context

M5 (#2) turns a push into a running app: fetch the source at the commit, build
the repository's Dockerfile, push the image to the region's registry, deploy it.
ADR 005 and M4 got a push as far as a `queued` deployment with no image.

What the build has to live with, as measured on the staging guest:

- Ubuntu 26.04, 4 vCPU, 11 GiB RAM, k3s v1.37 with containerd v2.3. The control
  plane, the registry, Traefik and every app share this one node.
- The registry is a plain-HTTP, unauthenticated Distribution pod: a ClusterIP
  Service for pods, and a hostPort on the node's loopback that containerd uses
  as the `localhost:5000` mirror. Deletion through its API is enabled.
- There is no CI (Actions has no credits), so everything is built and checked on
  the guest or on the owner's machine.
- The source is private. The App can mint an installation token that is limited
  to chosen repositories and to `contents: read`; it expires within an hour.
- A build runs the repository's own Dockerfile, so it runs arbitrary commands.
  The roadmap treats the app as the owner's own trusted code, but a build is
  where that code first runs next to the control plane, so what it can reach
  matters.

## What was measured

A throwaway namespace on the staging guest, on 2026-10-06, removed afterwards.
BuildKit image `moby/buildkit:rootless` (123 MB, digest `f8a833b2de9d6...`).

| Question | Result |
|---|---|
| Does rootless BuildKit run as a plain Job, not privileged, as uid 1000, with seccomp and AppArmor `Unconfined`? | Yes, although this kernel has `apparmor_restrict_unprivileged_userns=1`. A one-step build, including pulling the base image, took 20 seconds |
| Can it push to the registry from a pod? | Yes, over plain HTTP to the ClusterIP Service (`registry.insecure=true`), as `registry.registry.svc.cluster.local:5000/<repo>:<tag>` |
| Can the node run what it pushed? | Yes: containerd pulled it as `localhost:5000/<repo>:<tag>` through the existing mirror, so a deployment names the image that way |
| What does a `RUN` step run as? | root in a nested user namespace, with default container capabilities and a seccomp filter; it sees no host mounts, no service account token and no Docker socket |
| What can a `RUN` step reach by default? | The internet, the registry, vora-cloud's HTTP port on the node, the Kubernetes API and the kubelet. The API and kubelet answer 401 to an anonymous request |
| Does a NetworkPolicy fix that? | For everything but the registry: with egress limited to cluster DNS, the registry namespace on 5000 and the internet outside the private ranges, a build step could no longer reach the node, the API, the kubelet or vora-cloud ("connection refused"), and the build still pulled, built and pushed |
| What can a `RUN` step do to the registry? | Everything: with that policy in place, a step read another image's digest and **deleted its manifest** (HTTP 202). The registry has no authentication, and the push needs the same access |

Not tested: a private repository as the build context (the token mechanism is
BuildKit's documented `GIT_AUTH_TOKEN` secret, which uses `x-access-token` basic
authentication, the form an installation token takes), a rootful `buildkitd`,
cache export, and load (one build at a time was run).

## Options

**A. Rootless BuildKit, one Job per build.** Tested above. No daemon to keep
running, nothing privileged, the build's state goes away with the pod. The cost
is a cold cache on every build unless it is exported to the registry.

**B. A long-lived rootful `buildkitd`** (a systemd service or a DaemonSet).
Warm caches and one fewer image pull, but a root daemon on the node executes
every user's Dockerfile, and the build and the control plane share it. Not
tested, because A works.

**C. Kaniko.** Its repository was archived in June 2025 and is no longer
maintained.

**D. Buildpacks.** Framework detection is deferred by the roadmap. A Dockerfile
first.

## Decision (proposed)

1. **Rootless BuildKit as one Job per build**, in a namespace `vora-builds`, with
   a ResourceQuota and a LimitRange, and the BuildKit image pinned by digest
   (the release current when it is built, not `:rootless`).
2. **A NetworkPolicy on that namespace** with exactly the egress tested above.
   It is part of the deployment of the builder, and `verify.yml` checks it, so
   a build can never run without it.
3. **The source is fetched by BuildKit**, from `https://github.com/<repo>.git#<sha>`,
   with the token as the `GIT_AUTH_TOKEN` secret, not in a URL, an argument or an
   environment variable of the build. The token is minted per build, limited to
   the one repository and `contents: read`, kept in a Secret that the Job mounts
   as a file and that is deleted with the Job. Build logs are filtered for it.
4. **The image is `localhost:5000/<project slug>/<app>:<sha>`.** It is pushed
   through the registry Service and deployed under the mirror name. A deployment
   moves `queued` to `building` to `pushing` to `deploying`, which the state
   machine already allows. The deployer is called only after a successful push,
   so a failed build leaves the previous deployment serving.
5. **Dockerfile only**, at the repository root; a path in the project's build
   configuration comes later.
6. **Queue.** vora-cloud runs the builds: one at a time on this node, a timeout
   (the Job's `activeDeadlineSeconds`), cancellation by deleting the Job, and
   resource limits on the pod. A newer push to a project cancels its older queued
   or running build, as it already supersedes a queued deployment.
7. **Logs** are read from the Job's pod while it runs, stored with the build, and
   streamed to the dashboard over server-sent events. The stream that exists sends
   a project's statuses; the log has an event stream of its own per deployment
   (`.../deployments/:dep/log/stream`), because a page about one build should not
   receive every deployment's text. It reads the stored log through the project
   service, never the pod, so the owner check and the removal of tokens are the
   ones the page has, and it counts against the same per-session limit.
8. **Cache** is exported to the registry (`--export-cache type=registry`) and
   imported by the next build, so a Job per build is not a cold build.

## Decided by the owner (2026-10-06)

- **The registry stays writable by every build, for now.** Decision 2 closes the
  node and the cluster, but a Dockerfile in a project can still replace or delete
  another project's image, and a deployment would then run it. The owner accepts
  this for the closed beta, where the owner is the only user. It must be closed
  before a second user's code is built: either a credential per build that can
  write only that project's repository (a registry with token authentication), or
  a build that writes an image archive and a trusted step that pushes it.
- **Builds share the node** with the control plane and the apps, one at a time.
  Moving builds to their own node waits for the second region.
- **App hosts** (`<slug>.stg.cloud.vora.sh`) are not reachable from the internet
  until they have a Pangolin resource and a DNS record. A build can succeed and
  deploy and still not be visible. This is settled separately from the pipeline.

## Consequences

- A push can now end in a running app on the staging guest, once the builder, the
  namespace and the policy are in `vora-cloud-ansible` and the build code is in
  vora-cloud. Both are separate changes.
- Cold builds cost more (an image pull and a registry cache import) than they
  would on a warm daemon. This is the price of nothing privileged and nothing
  long-running.
- Every build holds a token and runs user code in one pod. The token is limited
  in what it can read and in time; the user code is limited in what it can reach.

## What the orchestration settled (2026-10-06)

Implemented in vora-cloud #48.

- **Deployed by digest.** When a build's Job succeeds, the service asks the
  registry for the digest of the pushed tag and deploys
  `localhost:5000/<project>/<project>@sha256:...`. Another build re-tagging the
  image cannot change what runs. The open registry still lets a build delete one.
- **How a repository is built.** A project's build configuration has a
  `dockerfile` (a path inside the build folder, default `Dockerfile`) and a
  `context` (a folder of the repository, default its root). A monorepo is several
  projects on one repository, each with its own. The create form asks for both,
  and for the port the app listens on. Paths stay inside the repository and carry
  nothing a BuildKit option list could take apart.
- **The cache is imported only if it exists.** Asking for one that does not makes
  BuildKit log an error, although the build goes on, and the first build of every
  project would show it.
- **State is in the database and in the Jobs.** A restart follows a running build,
  fails one whose Job is gone, and removes every Job and Secret that is not
  running. A build that something else ended (a manual deployment supersedes every
  unfinished one) has its Job and Secret removed on the next pass. A newer push
  cancels the build that is running for the project.
- **Commit statuses follow the deployment**, in order: building, pushing,
  deploying, then success, or failure with the reason. A commit queued while
  builds are off is still described as unable to be built.
- **The build log** is the tail (256 KiB) of what BuildKit printed, in a column of
  its own that deployment lists never read, with anything that looks like a GitHub
  token removed, served as plain text to the deployment's owner.
- **Off by default.** Builds start only when all of `BUILD_*` and the GitHub App
  are set.

Verified by hand on the staging guest, in a throwaway namespace, against a
private repository:

| What | Result |
|---|---|
| BuildKit fetches a private repository itself, with a token of one repository and `contents: read` as the `GIT_AUTH_TOKEN` secret from a mounted Secret | Works. The URL in the log has no credentials, and the token is in none of the log's 1,043 lines |
| `Dockerfile.backend` at the root, selected with `filename` | Built a Python backend and pushed it in 79 s, cold |
| `#<sha>:frontend` as a subfolder context, with a Dockerfile relative to it | Works |
| Registry cache export and import | The first build took 53 s, the second 8 s, with 11 cached steps |

Then through the orchestrator, on the staging guest, from a real push (vora-cloud
#48 with vora-cloud-ansible #17 and #18 applied):

| What | Result |
|---|---|
| A push to the default branch of the private repository, with a project set to `Dockerfile.backend` | Queued, built, pushed and deployed by digest in about 80 s. The node pulled `localhost:5000/<slug>/<slug>@sha256:...` from the registry mirror. The Job and Secret were gone afterwards, and no token-like string was in the 83 KB stored log |
| A project created before build settings existed (default `Dockerfile`, which the repository does not have) | The private fetch worked, the build failed in 7 s with BuildKit's error, the deployment was marked failed with it, the log was stored, and nothing was left in the namespace |
| Two projects on one repository, one push | One deployment each, built one at a time |

The app that was deployed then crashed on start for want of a database setting.
That is the app's configuration. The deployment stays `deploying` until the
10-minute rollout timeout of the project service fails it.
