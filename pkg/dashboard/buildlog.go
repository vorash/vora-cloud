package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vorash/vora-cloud/pkg/project"
)

// deploymentOf finds the deployment named in the path. It is the owner's alone:
// a deployment of another user, or one that is not the project in the path, is
// "not found", the same as one that does not exist.
func (d *Dashboard) deploymentOf(c echo.Context) (*project.Deployment, error) {
	pid, err := projectID(c)
	if err != nil {
		return nil, project.ErrNotFound
	}
	depID, err := uuid.Parse(c.Param("dep"))
	if err != nil {
		return nil, project.ErrNotFound
	}
	dep, err := d.cfg.Projects.GetDeployment(c.Request().Context(), sessionOf(c).UserID, depID)
	if err == nil && dep.ProjectID != pid {
		return nil, project.ErrNotFound
	}
	return dep, err
}

// building reports whether a deployment's build can still print something. A
// queued one has no log yet but may get one.
func building(s project.Status) bool {
	return s == project.StatusQueued || s == project.StatusBuilding || s == project.StatusPushing
}

// buildLog serves the log of a deployment's build as plain text. Plain text, with
// nosniff from secureHeaders, so what a Dockerfile printed cannot become a page.
func (d *Dashboard) buildLog(c echo.Context) error {
	dep, err := d.deploymentOf(c)
	if errors.Is(err, project.ErrNotFound) {
		return d.notFound(c)
	}
	if err != nil {
		return d.serverError(c, err)
	}
	text, err := d.cfg.Projects.BuildLog(c.Request().Context(), sessionOf(c).UserID, dep.ID)
	if err != nil {
		return d.serverError(c, err)
	}
	if text == "" {
		text = noBuildLog
	}
	return c.String(http.StatusOK, text)
}

const noBuildLog = "There is no build log for this deployment.\n"

type deploymentData struct {
	Project    *project.Project
	Deployment *project.Deployment
	// Log is what to show now; with scripts on, the stream replaces it.
	Log string
	// Streaming is set while the build can still print something; Empty, that
	// there is nothing to show yet but a note.
	Streaming, Empty bool
}

// deploymentPage shows a deployment's build log. The text is in the page, so it
// works without scripts; while the build runs, app.js follows the stream below.
func (d *Dashboard) deploymentPage(c echo.Context) error {
	dep, err := d.deploymentOf(c)
	if errors.Is(err, project.ErrNotFound) {
		return d.notFound(c)
	}
	if err != nil {
		return d.serverError(c, err)
	}
	ctx, user := c.Request().Context(), sessionOf(c).UserID
	p, err := d.cfg.Projects.GetProject(ctx, user, dep.ProjectID)
	if errors.Is(err, project.ErrNotFound) {
		return d.notFound(c)
	}
	if err != nil {
		return d.serverError(c, err)
	}
	text, err := d.cfg.Projects.BuildLog(ctx, user, dep.ID)
	if err != nil {
		return d.serverError(c, err)
	}
	streaming, empty := building(dep.Status), text == ""
	if empty {
		text = noBuildLog
		if streaming {
			text = "No output yet.\n"
		}
	}
	return d.render(c, http.StatusOK, "deployment", page{Title: "Build log", Data: deploymentData{Project: p, Deployment: dep, Log: text, Streaming: streaming, Empty: empty}})
}

// logFrame is one "log" event. Text is added to what the page has, or replaces it
// when Reset is set. It is JSON, so what a Dockerfile printed (a line that looks
// like an event, say) stays inside one event's data.
type logFrame struct {
	Text   string         `json:"text"`
	Reset  bool           `json:"reset,omitempty"`
	Status project.Status `json:"status"`
	Error  string         `json:"error,omitempty"`
}

// logAnchor is how much of the end of the last text identifies where it ends in
// a newer one. The stored log is a tail, so once it is full the beginning moves.
const logAnchor = 4096

// logMatches bounds the places prev's end is tried in a newer text, which a log of
// one repeated line would otherwise make a quadratic search.
const logMatches = 32

// logDelta says what a viewer who has prev needs to reach cur: the text that
// follows, and whether it must replace everything instead because prev's end is
// not in cur. When the tail has slid, what remains of prev is the start of cur;
// that is checked in full, not just at the anchor. The earliest such place is
// taken, so a repeated block can resend text but never drops any.
func logDelta(prev, cur string) (text string, reset bool) {
	if strings.HasPrefix(cur, prev) {
		return cur[len(prev):], false
	}
	anchor := prev
	if len(anchor) > logAnchor {
		anchor = anchor[len(anchor)-logAnchor:]
	}
	if anchor == "" {
		return cur, true
	}
	for from, tries := 0, 0; tries < logMatches; tries++ {
		j := strings.Index(cur[from:], anchor)
		if j < 0 {
			break
		}
		end := from + j + len(anchor)
		if kept := len(prev) - end; kept >= 0 && strings.HasPrefix(cur, prev[kept:]) {
			return cur[end:], false
		}
		from += j + 1
	}
	return cur, true
}

// logStream follows a build's log as server-sent events (ADR 006, decision 7). It
// reads the log that project.Service stores, never the pod, so the owner check
// and the removal of tokens are the ones the page has. The first event carries
// the whole log; later ones carry what was added. It ends when the build can
// print no more, even if the deployment goes on.
func (d *Dashboard) logStream(c echo.Context) error {
	var (
		prev       string
		prevStatus project.Status
		started    bool
	)
	return d.serveStream(c, "log", "log "+c.Param("dep"), func() (string, bool, error) {
		// The deployment is read first: its status can only be ahead of the log.
		dep, err := d.deploymentOf(c)
		if err != nil {
			return "", false, err
		}
		cur, err := d.cfg.Projects.BuildLog(c.Request().Context(), sessionOf(c).UserID, dep.ID)
		if err != nil {
			return "", false, err
		}
		final := !building(dep.Status)
		frame := logFrame{Status: dep.Status, Error: dep.Error}
		switch {
		case !started:
			frame.Text, frame.Reset = cur, true
		case cur == prev && dep.Status == prevStatus:
			return "", final, nil
		default:
			frame.Text, frame.Reset = logDelta(prev, cur)
		}
		started, prev, prevStatus = true, cur, dep.Status
		b, err := json.Marshal(frame)
		return string(b), final, err
	})
}
