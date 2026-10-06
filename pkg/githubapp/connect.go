package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/google/go-github/v92/github"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	oauthgithub "golang.org/x/oauth2/github"
)

var (
	// ErrAuthorization means GitHub refused the authorization code: it is wrong,
	// used or older than ten minutes. The user starts again.
	ErrAuthorization = errors.New("GitHub did not accept the authorization")
	// ErrNotInstalled means the App is not installed on the user's own GitHub
	// account. Organizations are not supported, so an installation on one does
	// not count.
	ErrNotInstalled = errors.New("the App is not installed on this GitHub account")
	// ErrInstallationTaken means another Vora user already connected the
	// installation.
	ErrInstallationTaken = errors.New("the installation is connected to another account")
	// ErrNotFound is returned for an installation that is not the user's, the
	// same as one that does not exist, so ids cannot be probed.
	ErrNotFound = errors.New("not found")
)

// Installation is a GitHub account where the App is installed, connected to a
// Vora user.
type Installation struct {
	ID           int64
	UserID       uuid.UUID
	AccountID    int64
	AccountLogin string
}

// Links stores which Vora user an installation belongs to.
type Links interface {
	// Link connects an installation to a user. Linking it again for the same
	// user changes nothing; for another user it returns ErrInstallationTaken.
	Link(ctx context.Context, i Installation) error
	List(ctx context.Context, userID uuid.UUID) ([]Installation, error)
	// Get returns ErrNotFound unless the installation is the user's.
	Get(ctx context.Context, userID uuid.UUID, installationID int64) (*Installation, error)
}

// Repository is a repository the App can read.
type Repository struct {
	ID            int64
	FullName      string
	Private       bool
	DefaultBranch string
}

// Repositories is one page of an installation's repositories.
type Repositories struct {
	List []Repository
	// Total is how many the installation can read, which can exceed the list.
	Total int
}

// reposPerPage is the most GitHub returns at once.
const reposPerPage = 100

// maxRepoPages bounds the search of Repository: 50 pages of 100 repositories.
const maxRepoPages = 50

// Connector connects Vora users to their GitHub installations.
type Connector struct {
	app   *App
	links Links
	oauth *oauth2.Config
	slug  string
}

// NewConnector returns the connector. redirectURL is the App's callback URL,
// exactly as registered with GitHub; empty uses the first registered one.
func NewConnector(app *App, links Links, slug, clientID, clientSecret, redirectURL string) *Connector {
	endpoint := oauthgithub.Endpoint
	// The client secret goes in the request body, as GitHub documents.
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	return &Connector{
		app: app, links: links, slug: slug,
		oauth: &oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, Endpoint: endpoint, RedirectURL: redirectURL},
	}
}

// AuthorizeURL is where GitHub asks the user to authorize the App. It always
// comes back to the callback with the state, whether or not the App is
// installed.
func (c *Connector) AuthorizeURL(state string) string {
	return c.oauth.AuthCodeURL(state)
}

// InstallURL is where the user installs the App.
func (c *Connector) InstallURL(state string) string {
	u := url.URL{Scheme: "https", Host: "github.com", Path: "/apps/" + c.slug + "/installations/new"}
	u.RawQuery = url.Values{"state": {state}}.Encode()
	return u.String()
}

// Link connects the user to the installation on their own GitHub account.
//
// The code proves who the GitHub user is. Nothing else in the callback is
// trusted, installation_id included: anyone can put any id in that URL. So the
// installations come from GitHub, for that user, and only the one on their
// personal account is linked. An organization's installation is not: a member
// who can see one repository would otherwise reach every repository of it.
//
// The user's token lives only inside this call; it is never stored or logged.
func (c *Connector) Link(ctx context.Context, userID uuid.UUID, code string) (*Installation, error) {
	if code == "" {
		return nil, ErrAuthorization
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: apiTimeout})
	tok, err := c.oauth.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAuthorization, err)
	}
	gh, err := c.app.clientFor(tok.AccessToken)
	if err != nil {
		return nil, err
	}
	me, _, err := gh.Users.Get(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("githubapp: reading the GitHub user: %w", err)
	}
	installs, _, err := gh.Apps.ListUserInstallations(ctx, &github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, fmt.Errorf("githubapp: listing the user's installations: %w", err)
	}
	for _, in := range installs {
		// The account's id, not its login, which can be renamed or reused.
		if acc := in.GetAccount(); acc.GetType() == "User" && acc.GetID() == me.GetID() {
			link := Installation{ID: in.GetID(), UserID: userID, AccountID: acc.GetID(), AccountLogin: acc.GetLogin()}
			if err := c.links.Link(ctx, link); err != nil {
				return nil, err
			}
			return &link, nil
		}
	}
	return nil, ErrNotInstalled
}

// Installations lists the user's connected installations.
func (c *Connector) Installations(ctx context.Context, userID uuid.UUID) ([]Installation, error) {
	return c.links.List(ctx, userID)
}

// Repositories lists what the App can read through one of the user's
// installations. It is ErrNotFound for any other installation, checked before
// a token is minted.
func (c *Connector) Repositories(ctx context.Context, userID uuid.UUID, installationID int64) (*Repositories, error) {
	if _, err := c.links.Get(ctx, userID, installationID); err != nil {
		return nil, err
	}
	gh, err := c.app.InstallationClient(ctx, installationID)
	if err != nil {
		return nil, err
	}
	got, _, err := gh.Apps.ListRepos(ctx, &github.ListOptions{PerPage: reposPerPage})
	if err != nil {
		return nil, fmt.Errorf("githubapp: listing the installation's repositories: %w", err)
	}
	out := &Repositories{Total: got.GetTotalCount()}
	for _, r := range got.Repositories {
		out.List = append(out.List, repositoryOf(r))
	}
	return out, nil
}

func repositoryOf(r *github.Repository) Repository {
	return Repository{ID: r.GetID(), FullName: r.GetFullName(), Private: r.GetPrivate(), DefaultBranch: r.GetDefaultBranch()}
}

// Repository returns one repository that the user's installation can read, as
// GitHub describes it. It is ErrNotFound unless the installation is the user's
// and the repository is in that installation's own list.
//
// The list is searched, not the repository fetched by id: any token can read a
// public repository, so a fetch would also "find" a stranger's, or one the
// installation was never given. The caller sends only ids, and everything else
// (the name, the branch) comes from this answer.
func (c *Connector) Repository(ctx context.Context, userID uuid.UUID, installationID, repoID int64) (*Repository, error) {
	if _, err := c.links.Get(ctx, userID, installationID); err != nil {
		return nil, err
	}
	gh, err := c.app.InstallationClient(ctx, installationID)
	if err != nil {
		return nil, err
	}
	opts := &github.ListOptions{PerPage: reposPerPage}
	for range maxRepoPages {
		got, resp, err := gh.Apps.ListRepos(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("githubapp: listing the installation's repositories: %w", err)
		}
		for _, r := range got.Repositories {
			if r.GetID() == repoID {
				found := repositoryOf(r)
				return &found, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return nil, ErrNotFound
}
