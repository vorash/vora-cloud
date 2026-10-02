package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vorash/vora-cloud/pkg/project"
)

// streamWriteTimeout bounds one write, so a client that stops reading cannot
// hold a stream open.
const streamWriteTimeout = 10 * time.Second

// liveDeployment is what the page needs to redraw a deployment.
type liveDeployment struct {
	ID     string         `json:"id"`
	Status project.Status `json:"status"`
	Error  string         `json:"error,omitempty"`
}

// events streams the states of a project's newest deployments as server-sent
// events (ADR 004). It asks project.Service like the page does, so another
// user's project is a 404 here too, and it checks the session on every poll, so
// signing out ends the stream. It sends a "status" event on connect and when
// something changes, and "done" once every deployment is final, which tells the
// page to close rather than let the browser reconnect.
func (d *Dashboard) events(c echo.Context) error {
	id, err := projectID(c)
	if err != nil {
		return d.notFound(c)
	}
	ctx := c.Request().Context()
	var cookieValue string
	if cookie, err := c.Cookie(cookieName); err == nil {
		cookieValue = cookie.Value
	}
	hash := hashToken(cookieValue)

	// The first read happens before any header is written, so a project that is
	// not the caller's gets the normal 404.
	last, final, err := d.liveState(c, id)
	switch {
	case errors.Is(err, project.ErrNotFound):
		return d.notFound(c)
	case err != nil:
		return d.serverError(c, err)
	}
	if !d.openStream(hash) {
		return c.NoContent(http.StatusTooManyRequests)
	}
	defer d.closeStream(hash)

	h := c.Response().Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no") // a buffering proxy would hold the events back
	c.Response().WriteHeader(http.StatusOK)
	rc := http.NewResponseController(c.Response())
	if err := rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil {
		// Without it a client that stops reading holds the stream until the age limit.
		log.Printf("dashboard events: no write deadline on this connection: %v", err)
	}
	send := func(event, data string) error {
		_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		if _, err := fmt.Fprintf(c.Response(), "event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		return rc.Flush()
	}

	if send("status", last) != nil {
		return nil
	}
	poll, ping, limit := time.NewTicker(d.pollEvery), time.NewTicker(d.pingEvery), time.NewTimer(d.maxStreamAge)
	defer poll.Stop()
	defer ping.Stop()
	defer limit.Stop()
	for !final {
		select {
		case <-ctx.Done():
			return nil
		case <-d.stop:
			return nil
		case <-limit.C: // the browser reconnects and gets a fresh snapshot
			return nil
		case <-ping.C:
			_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
			if _, err := fmt.Fprint(c.Response(), ": ping\n\n"); err != nil || rc.Flush() != nil {
				return nil
			}
		case <-poll.C:
			if sess, err := d.cfg.Sessions.Get(ctx, hash); err != nil || sess == nil {
				return nil
			}
			now, nowFinal, err := d.liveState(c, id)
			if err != nil {
				if !errors.Is(err, project.ErrNotFound) && ctx.Err() == nil {
					log.Printf("dashboard events %s: %v", id, err)
				}
				return nil
			}
			final = nowFinal
			if now != last {
				last = now
				if send("status", now) != nil {
					return nil
				}
			}
		}
	}
	_ = send("done", "{}")
	return nil
}

// liveState reads the project's newest deployments as the JSON of a "status"
// event, and says whether every one of them is final.
func (d *Dashboard) liveState(c echo.Context, id uuid.UUID) (data string, final bool, err error) {
	page, err := d.cfg.Projects.ListDeployments(c.Request().Context(), sessionOf(c).UserID, id, project.Page{Limit: deploymentsPerPage})
	if err != nil {
		return "", false, err
	}
	out := struct {
		Deployments []liveDeployment `json:"deployments"`
	}{Deployments: make([]liveDeployment, 0, len(page.Deployments))}
	final = true
	for _, dep := range page.Deployments {
		out.Deployments = append(out.Deployments, liveDeployment{ID: dep.ID.String(), Status: dep.Status, Error: dep.Error})
		final = final && dep.Status.Final()
	}
	b, err := json.Marshal(out)
	return string(b), final, err
}

// openStream counts a stream against its session; false means the session has
// too many open.
func (d *Dashboard) openStream(session string) bool {
	d.streamsMu.Lock()
	defer d.streamsMu.Unlock()
	if d.streams[session] >= d.maxStreams {
		return false
	}
	d.streams[session]++
	return true
}

func (d *Dashboard) closeStream(session string) {
	d.streamsMu.Lock()
	defer d.streamsMu.Unlock()
	if d.streams[session]--; d.streams[session] <= 0 {
		delete(d.streams, session)
	}
}
