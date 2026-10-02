# ADR 004 - Dashboard stack and sign-up

- Status: accepted
- Date: 2026-09-30
- Amended: 2026-10-02 (decision 1 and decision 6, for #34)

ADR 003 is reserved for the "systemd beside k3s or inside the cluster" decision
in #16; this one took the next free number.

## Context

The dashboard (M8) is where users sign in, create projects and see
deployments. It runs on the same node as Postgres and the registry, and the
service can deploy any image a user names, so what a stolen session or an open
sign-up can do is large. Nothing here is "100% secure"; the goal is the
smallest attack surface that is easy to build.

## Decision

**1. Server-rendered pages from the vora-cloud binary** (Go `html/template`,
which escapes by context), on the same origin as the API (`cloud.vora.sh`). No
JavaScript build, no second deploy, no CORS, and no token that page scripts
can read. Every page works with scripts off; decision 6 adds one small script
that only improves them.

**2. Browser sessions are opaque.** Signing in creates a random 256-bit token,
stored only as its SHA-256 hash (like refresh tokens), in a cookie that is
`HttpOnly; Secure; SameSite=Strict`. It is not the API's JWT: the API keeps
JWTs and API keys for programs, the dashboard keeps sessions for browsers.
Sessions last 12 hours, sign-out deletes the row, and a session of a
deactivated user stops working at once (the lookup requires an active user).
Ending sessions on a password change is not built yet.

**3. Writes are checked twice against cross-site requests:** the `Origin`
header must be the site's own origin (`PUBLIC_URL`; a missing Origin is
refused), and every form carries a per-session CSRF token. The API's CORS
setting is permissive but never allows credentials, and the cookie is
`SameSite=Strict`, so no other site can make a browser send it.

**4. Sign-in is hardened the same way over the dashboard and the REST API**
(`POST /auth/login` is public on the same host, so guarding only one would
leave the other as the way in). An unknown email, a wrong password and an
inactive account get one answer and cost the same bcrypt work. Attempts are
limited per account, five and then one every 12 seconds, from **one budget
shared by both endpoints**, so switching endpoints buys no guesses. At most
four password checks run at once, which bounds the CPU an anonymous caller can
use. A limit per source address belongs to the edge (Traefik or Pangolin):
behind a proxy the app sees only the proxy's address, and forwarded headers
can be forged. The cost is that someone can slow a known user's sign-in by
guessing at it.

**5. Sign-up is closed.** `POST /auth/register` returns 403 unless
`ALLOW_REGISTRATION=true`; users are created with an admin API key
(`POST /admin/users`). Reopening it waits for per-project quotas and
network policies (M6), because a user can run arbitrary images on the shared
node.

**6. One first-party script, as progressive enhancement** (amended for #34).
The Content-Security-Policy gains `script-src 'self'` and `connect-src 'self'`,
and nothing else: no inline script, no `unsafe-inline`, nothing from another
origin. The script is a single file, embedded like the stylesheet and served
from `/assets/` under a content-hash URL. It has no build step and no
dependency, and it never writes HTML (it sets text and classes). It adds live
deployment status, a copy button for the project's address, a busy state on
Deploy and Redeploy, a confirmation after them, and relative times that tick.
Without it the pages are as they were, and the note that status updates on
reload stays true.

Live status is a server-sent-events endpoint, `GET /projects/:id/events`:

- It uses the session cookie and asks `project.Service`, so the ownership rule
  is the page's. Another user's project, an unknown one and a malformed id all
  answer 404, before any stream starts. The session cookie is `HttpOnly`, so
  the script cannot read it, and there is still no token in the page.
- It re-checks the session on every poll, so signing out (or deactivating the
  user) ends an open stream, in line with decision 2.
- A session may hold four streams (`429` beyond that). Each write has a
  deadline, a comment is sent every 20 seconds, and a stream ends after ten
  minutes (the browser reconnects and gets a fresh state) and when the server
  shuts down, so a proxy or a stalled client cannot hold one forever.
- It sends the current state at once, then only changes. When every deployment
  is final it sends `done` and closes, and the script closes its side so the
  browser does not reconnect. The page offers the stream only while the newest
  deployment is in progress.
- It polls the database every three seconds, so a change shows within about
  that time after the reconciler (every five seconds) settles a rollout. One
  poll per open stream is a few indexed reads; the cap above bounds them.

## Alternatives

- **A static single-page app** calling the API with the JWT needs a JavaScript
  build and keeps tokens where any injected script can read them.
- **A separate frontend app** adds a second host, CORS and browser-side token
  handling for no benefit at this size.

## Consequences

- Dashboard routes carry their middleware one by one, not through an empty
  route group, because echo answers every unmatched path under a group that has
  middleware; a group would turn each unknown URL into a redirect to sign-in.
  Request bodies are limited to 64 KB.
- The pages stay usable without scripts. Motion is CSS only and off under
  `prefers-reduced-motion`.
- The policy also allows the origin's own fonts and images
  (`font-src 'self'; img-src 'self'`), and, since decision 6, its own script and
  requests (`script-src 'self'; connect-src 'self'`), and nothing else: the
  stylesheet, the script, two fonts and the logo are embedded in the binary and
  served from `/assets/`. No `data:` images, no inline styles. The fonts carry licences that limit
  where the binary may go (see the README).
- The dashboard's handlers use the same `project.Service` as REST and gRPC, so
  the ownership rule is not duplicated.
- The dashboard is its own package (`pkg/dashboard`) with its own session table
  (migration 006), and can be moved to a separate service later without
  touching the API.
- A failed deployment's error is a short reason chosen by the project service
  (rollout timed out, image not pulled, cluster unreachable, spec refused, or a
  generic text with the deployment id). The cluster's own text can name
  internal addresses, so it goes to the log only.
