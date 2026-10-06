package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vorash/vora-cloud/pkg/githubapp"
)

// GitHub is the connection to the GitHub App (githubapp.Connector). Config
// leaves it nil to switch the whole feature off.
type GitHub interface {
	AuthorizeURL(state string) string
	InstallURL(state string) string
	Link(ctx context.Context, userID uuid.UUID, code string) (*githubapp.Installation, error)
	Installations(ctx context.Context, userID uuid.UUID) ([]githubapp.Installation, error)
	Repositories(ctx context.Context, userID uuid.UUID, installationID int64) (*githubapp.Repositories, error)
	// Repository is ErrNotFound unless the installation is the user's and the
	// repository is one it can read.
	Repository(ctx context.Context, userID uuid.UUID, installationID, repoID int64) (*githubapp.Repository, error)
}

// stateLifetime is how long a connection attempt stays valid: GitHub's
// authorization code lives ten minutes.
const stateLifetime = 10 * time.Minute

// githubState is the value that goes to GitHub and comes back with the code. It
// is bound to the session that started the connection, so a link someone else
// built (for example one that makes a victim authorize and so connect the
// victim's GitHub account to the attacker) is refused: the session cannot be
// forged, and the state is useless in any other session. It carries an expiry
// and a MAC, nothing else, and no CSRF token, since it travels through GitHub.
func (d *Dashboard) githubState(tokenHash string) string {
	exp := strconv.FormatInt(d.now().Add(stateLifetime).Unix(), 10)
	return exp + "." + d.githubStateMAC(tokenHash, exp)
}

func (d *Dashboard) validGitHubState(tokenHash, state string) bool {
	exp, mac, ok := strings.Cut(state, ".")
	if !ok {
		return false
	}
	until, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || d.now().Unix() > until {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(d.githubStateMAC(tokenHash, exp)))
}

func (d *Dashboard) githubStateMAC(tokenHash, exp string) string {
	mac := hmac.New(sha256.New, d.cfg.CSRFKey)
	mac.Write([]byte("github-connect\x00" + tokenHash + "\x00" + exp))
	return hex.EncodeToString(mac.Sum(nil))
}

// githubConnect sends the user to GitHub to authorize the App. It is a link,
// not a form: the page's form-action rule would block a form that ends up on
// github.com.
func (d *Dashboard) githubConnect(c echo.Context) error {
	return c.Redirect(http.StatusSeeOther, d.cfg.GitHub.AuthorizeURL(d.githubState(c.Get(tokenHashKey).(string))))
}

// githubCallback is where GitHub sends the user back. That arrives from another
// site, so the browser withholds the SameSite=Strict session cookie, and this
// request cannot tell who the user is. It therefore does nothing but pass the
// code and the state on to githubFinish, in a navigation that starts from this
// site's own page, which carries the cookie. Only those two values are passed:
// installation_id and everything else a caller can put in the URL is dropped.
func (d *Dashboard) githubCallback(c echo.Context) error {
	q := c.QueryParams()
	if q.Get("error") != "" {
		return d.errorPage(c, http.StatusBadRequest, "GitHub was not authorized. You can start again from the GitHub page.")
	}
	next := "/github/finish?" + url.Values{"code": {q.Get("code")}, "state": {q.Get("state")}}.Encode()
	c.Response().Header().Set("Refresh", "0; url="+next)
	return d.render(c, http.StatusOK, "github_callback", page{Title: "Connecting GitHub", Data: next})
}

// githubFinish connects the user's GitHub account, if the state is the one
// this session started and the code is good.
func (d *Dashboard) githubFinish(c echo.Context) error {
	if !d.validGitHubState(c.Get(tokenHashKey).(string), c.QueryParam("state")) {
		return d.githubPage(c, http.StatusForbidden, "This connection could not be verified. Start again with Connect GitHub.", "")
	}
	_, err := d.cfg.GitHub.Link(c.Request().Context(), sessionOf(c).UserID, c.QueryParam("code"))
	switch {
	case errors.Is(err, githubapp.ErrAuthorization):
		return d.githubPage(c, http.StatusBadRequest, "GitHub did not accept the authorization. Start again with Connect GitHub.", "")
	case errors.Is(err, githubapp.ErrNotInstalled):
		return d.githubPage(c, http.StatusUnprocessableEntity, "The Vora Cloud app is not installed on your personal GitHub account. Install it, then come back. Organizations are not supported yet.", d.githubState(c.Get(tokenHashKey).(string)))
	case errors.Is(err, githubapp.ErrInstallationTaken):
		return d.githubPage(c, http.StatusConflict, "That GitHub account is already connected to another Vora account.", "")
	case err != nil:
		return d.serverError(c, err)
	}
	return c.Redirect(http.StatusSeeOther, "/github")
}

// repoChoice is the GitHub repository a project is being created from. Only the
// two ids come from the request; the rest is what GitHub said about them.
type repoChoice struct {
	InstallationID, ID int64
	FullName           string
	DefaultBranch      string
	Private            bool
	// Unconfirmed is set when GitHub could not be asked: the ids are kept so the
	// form can be sent again, and nothing else is known.
	Unconfirmed bool
}

// repoFor checks the repository a request names. installation and repo are the
// raw parameters; anything that is not a positive number is "not found", the
// same as a repository that is not the user's.
func (d *Dashboard) repoFor(c echo.Context, installation, repo string) (*repoChoice, error) {
	in, err1 := strconv.ParseInt(installation, 10, 64)
	id, err2 := strconv.ParseInt(repo, 10, 64)
	if err1 != nil || err2 != nil || in <= 0 || id <= 0 {
		return nil, githubapp.ErrNotFound
	}
	r, err := d.cfg.GitHub.Repository(c.Request().Context(), sessionOf(c).UserID, in, id)
	if err != nil {
		if errors.Is(err, githubapp.ErrNotFound) {
			return nil, err
		}
		log.Printf("dashboard: checking repository %d of installation %d: %v", id, in, err)
		return &repoChoice{InstallationID: in, ID: id, Unconfirmed: true}, err
	}
	return &repoChoice{InstallationID: in, ID: id, FullName: r.FullName, DefaultBranch: r.DefaultBranch, Private: r.Private}, nil
}

var slugRuns = regexp.MustCompile(`[^a-z0-9]+`)

// slugFor suggests a project slug from a repository's name: lowercase letters
// and digits, runs of anything else turned into one dash, at most 61
// characters, and no dash at either end. It may be empty.
func slugFor(repoName string) string {
	s := strings.Trim(slugRuns.ReplaceAllString(strings.ToLower(repoName), "-"), "-")
	if len(s) > 61 {
		s = strings.Trim(s[:61], "-")
	}
	return s
}

type githubData struct {
	Installations []githubInstallation
	// InstallURL, when set, is the link to install the App.
	InstallURL string
}

type githubInstallation struct {
	ID      int64
	Account string
	Repos   *githubapp.Repositories
	// Failed is set when GitHub could not list this installation's repositories.
	Failed bool
}

func (d *Dashboard) githubIndex(c echo.Context) error {
	return d.githubPage(c, http.StatusOK, "", "")
}

// githubPage renders the GitHub page; a non-empty installState adds the link to
// install the App.
func (d *Dashboard) githubPage(c echo.Context, status int, msg, installState string) error {
	ctx := c.Request().Context()
	user := sessionOf(c).UserID
	installs, err := d.cfg.GitHub.Installations(ctx, user)
	if err != nil {
		return d.serverError(c, err)
	}
	data := githubData{}
	if installState != "" {
		data.InstallURL = d.cfg.GitHub.InstallURL(installState)
	}
	for _, in := range installs {
		row := githubInstallation{ID: in.ID, Account: in.AccountLogin}
		repos, err := d.cfg.GitHub.Repositories(ctx, user, in.ID)
		if err != nil {
			// GitHub being slow or down must not take the page with it.
			log.Printf("dashboard: listing repositories of installation %d: %v", in.ID, err)
			row.Failed = true
		}
		row.Repos = repos
		data.Installations = append(data.Installations, row)
	}
	return d.render(c, status, "github", page{Title: "GitHub", Error: msg, Data: data, OnGitHub: true})
}
