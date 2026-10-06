# ADR 005 - Connecting a user's GitHub account

- Status: accepted
- Date: 2026-10-05
- Amended: 2026-10-06 (decisions 6 and 7: a project from a repository, and pushes)

## Context

A user connects GitHub from the dashboard so Vora can read their repositories
(M4, #20). GitHub sends the user back to our callback URL with a `code`. That
redirect has two problems this ADR settles.

The callback is a navigation from github.com, so the browser withholds the
session cookie, which ADR 004 makes `SameSite=Strict`. And anything in the
callback URL can be forged: GitHub's own documentation says not to trust
`installation_id`, because anyone can put any id there.

## Decision

**1. The callback does nothing but hop.** `GET /github/callback` needs no
session. It answers with a `Refresh` header and a link to `/github/finish`,
carrying only `code` and `state` and dropping every other parameter. The
browser then navigates from this site's own page, which counts as same-site,
so the `Strict` cookie is sent and `/github/finish` knows the user. The
cookie keeps `Strict` (ADR 004). Checked in Chromium 152 and Firefox 155: from
another site a link straight to `/github/finish` ends at the sign-in page,
and the same link through the callback arrives signed in.

**2. The state belongs to the session.** `/github/connect` sends the user to
GitHub's authorize page with `state = expiry + HMAC(session token hash,
expiry)`, valid ten minutes like the code. `/github/finish` refuses any other
state. Without this, someone could build a link that makes a victim authorize,
and the victim's GitHub account would be connected to the attacker's Vora
account. The state carries no CSRF token, because it passes through GitHub.

**3. Who the user is comes from GitHub, not the URL.** The code is exchanged
for a user token. The service asks GitHub for that user and for the
installations that user can see, and ignores `installation_id`. The token is
used inside the request and never stored or logged.

**4. Personal accounts only.** An installation is linked only if its account
is of type `User` and has the GitHub user's own id (ids, because logins can be
renamed and reused). A user can see an organization's installation if they can
see one of its repositories, and linking it would let them read every
repository it covers. Organizations need their own design: check that the user
is an owner, or list repositories with the user's token. #20 names the owner of
the App as an account, not an org, so this is enough to start.

**5. One owner per installation.** The installation id is the primary key of
`github_installations`; a second Vora user who tries to link it is refused, and
the row is never reassigned.

**6. A project is tied to its repository by ids, checked on the server.**
The dashboard creates a project from a repository the user picked on the GitHub
page. The request carries only two numbers, the installation and the
repository. The server checks that the installation is the user's, then finds
the repository in that installation's own list (paged, by id) and takes the
name and default branch from GitHub's answer, never from the form. It does not
ask GitHub for the repository by id: any token can read any public repository,
so that would accept a stranger's. The two ids are stored on the project
(`github_installation_id` and `github_repo_id`, both set or neither, enforced
by the database) and are the only link. Nothing in the REST or gRPC API can set
them, and a test keeps it so. Those APIs still take a free-text `repo`, which is
only a label: it is not shown as a connected repository and must never be used
to route a push, which would let anyone claim another person's `owner/repo`.
Pushes are matched on the installation id and the repository id.

**7. A push to the default branch queues a deployment, routed on ids.**
The webhook acts on a `push` only when its ref is the repository's default
branch as the payload names it now (not the one stored when the project was
created), the branch is not being deleted, and the commit id and repository
name are well formed (they end up in a URL path). It finds projects by the
installation id and repository id, never by the free-text `repo`. A deployment
is queued with no image; nothing builds one yet, so it stays `queued`, and a
newer push fails an older queued one as superseded, so a redelivery or two
quick pushes leave one. The delivery is recorded before it is handled and
forgotten if handling fails, so GitHub's redelivery is handled and a copy after
success is not. The commit status (`pending`, context `vora/<slug>`) is posted
after the webhook has answered, because GitHub gives a delivery ten seconds, and
a failure to post it is logged, not retried through the webhook: the deployments
exist either way, and a redelivery would only queue them again.

## Consequences

- The page does a second navigation after GitHub. It is a redirect the user
  barely sees, and works with scripts off.
- The echo request logger records the whole URL, so `code` and `state` reach
  the logs. The code is single-use, expires in ten minutes and needs the client
  secret to be redeemed. Removing the query from the log is a separate change.
- An uninstalled App leaves its row behind until the `installation` webhook is
  processed (a later step); using the stale link fails when a token is
  requested.
- Organization installations are refused with a message, and the dashboard
  lists repositories through the installation, which is safe only because of 4.
- The default branch is stored when the project is created. A rename on GitHub
  leaves it stale until a later step reads it from each push.
- One repository can back several projects (for example the apps of a
  monorepo); a push will then reach each of them.
- A project that already exists cannot be linked to a repository yet.
- Until builds exist, every push to a linked repository's default branch leaves a
  `pending` status "Queued. Builds are not available yet." on its commit.
