package dashboard

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/project"
)

// stream is an open connection to an events endpoint, read event by event.
type stream struct {
	resp   *http.Response
	events chan string // "name data" per event; closed when the server ends the stream
	cancel context.CancelFunc
}

func (v *env) openStream(t *testing.T, srv *httptest.Server, path string, cookie *http.Cookie) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	s := &stream{resp: resp, events: make(chan string, 16), cancel: cancel}
	go func() {
		defer close(s.events)
		var name string
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			switch line := sc.Text(); {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				s.events <- name + " " + strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return s
}

// next waits for the next event, or reports that the stream ended.
func (s *stream) next(t *testing.T) (string, bool) {
	t.Helper()
	select {
	case e, ok := <-s.events:
		return e, ok
	case <-time.After(3 * time.Second):
		t.Fatal("no event within 3s")
		return "", false
	}
}

// eventsEnv is an environment with a server on a real socket, a fast poll, and
// alice's project with one deployment in progress.
func eventsEnv(t *testing.T) (v *env, srv *httptest.Server, alice *http.Cookie, projectID uuid.UUID, dep *project.Deployment) {
	t.Helper()
	v = newEnv(t)
	v.dash.pollEvery = 10 * time.Millisecond
	srv = httptest.NewServer(v.e)
	t.Cleanup(srv.Close)
	alice = v.login(t, "alice@example.com", "alice-pass")
	projectID = uuid.MustParse(strings.TrimPrefix(v.createProject(t, alice, "Web", "web"), "/projects/"))
	dep, err := v.projects.Deploy(context.Background(), aliceID, projectID, project.DeployInput{Image: "reg/web:1"})
	if err != nil {
		t.Fatal(err)
	}
	return v, srv, alice, projectID, dep
}

func (v *env) settle(t *testing.T, dep *project.Deployment, to project.Status, reason string) {
	t.Helper()
	if err := v.store.Move(context.Background(), dep.ID, dep.Status, to, reason); err != nil {
		t.Fatal(err)
	}
	dep.Status = to
}

// A rollout that finishes shows on an open page: the stream sends the first
// state, the change when the reconciler settles it, and then closes itself.
func TestEventsReportARolloutAndThenEnd(t *testing.T) {
	v, srv, alice, id, dep := eventsEnv(t)
	s := v.openStream(t, srv, "/projects/"+id.String()+"/events", alice)
	if ct := s.resp.Header.Get("Content-Type"); s.resp.StatusCode != 200 || ct != "text/event-stream" {
		t.Fatalf("%d %q", s.resp.StatusCode, ct)
	}
	if got, _ := s.next(t); !strings.HasPrefix(got, "status ") || !strings.Contains(got, `"status":"deploying"`) || !strings.Contains(got, dep.ID.String()) {
		t.Errorf("first event: %s", got)
	}

	v.settle(t, dep, project.StatusFailed, "rollout timed out")
	if got, _ := s.next(t); !strings.Contains(got, `"status":"failed"`) || !strings.Contains(got, `"error":"rollout timed out"`) {
		t.Errorf("after the rollout failed: %s", got)
	}
	// "done" is what makes the page close instead of letting the browser reconnect.
	if got, _ := s.next(t); got != "done {}" {
		t.Errorf("after every deployment is final: %q, want done", got)
	}
	if got, ok := s.next(t); ok {
		t.Errorf("the stream stayed open after done: %q", got)
	}
}

// A project that finished between the page render and the connection still
// gets its final state, so the page does not stay stale.
func TestEventsOnAFinishedProjectSendTheStateAndEnd(t *testing.T) {
	v, srv, alice, id, dep := eventsEnv(t)
	v.settle(t, dep, project.StatusReady, "")
	s := v.openStream(t, srv, "/projects/"+id.String()+"/events", alice)
	if got, _ := s.next(t); !strings.Contains(got, `"status":"ready"`) {
		t.Errorf("first event: %s", got)
	}
	if got, _ := s.next(t); got != "done {}" {
		t.Errorf("second event: %q, want done", got)
	}
}

// The ownership rule is the page's: someone else's project, an unknown one and
// a malformed id all answer 404, and no stream starts.
func TestEventsOfAnotherUsersProjectAre404(t *testing.T) {
	v, srv, _, id, _ := eventsEnv(t)
	bob := v.login(t, "bob@example.com", "bob-pass")
	for _, path := range []string{"/projects/" + id.String() + "/events", "/projects/" + uuid.NewString() + "/events", "/projects/nope/events"} {
		resp, err := srv.Client().Do(func() *http.Request {
			r, _ := http.NewRequest("GET", srv.URL+path, nil)
			r.AddCookie(bob)
			return r
		}())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Content-Type") == "text/event-stream" {
			t.Errorf("%s: %d %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
}

func TestEventsNeedASession(t *testing.T) {
	_, srv, _, id, _ := eventsEnv(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(srv.URL + "/projects/" + id.String() + "/events")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("anonymous: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// Signing out must cut off a stream that is already open (ADR 004: a session
// stops working at once).
func TestEventsEndWhenTheSessionEnds(t *testing.T) {
	v, srv, alice, id, _ := eventsEnv(t)
	s := v.openStream(t, srv, "/projects/"+id.String()+"/events", alice)
	s.next(t)
	if err := v.sessions.Delete(context.Background(), hashToken(alice.Value)); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.next(t); ok {
		t.Errorf("the stream outlived its session: %q", got)
	}
}

func TestEventsEndAtTheirMaximumAge(t *testing.T) {
	v, srv, alice, id, _ := eventsEnv(t)
	v.dash.maxStreamAge = 50 * time.Millisecond
	s := v.openStream(t, srv, "/projects/"+id.String()+"/events", alice)
	s.next(t)
	if got, ok := s.next(t); ok {
		t.Errorf("a stream past its maximum age sent %q", got)
	}
}

func TestEventsEndWhenTheServerShutsDown(t *testing.T) {
	v, srv, alice, id, _ := eventsEnv(t)
	s := v.openStream(t, srv, "/projects/"+id.String()+"/events", alice)
	s.next(t)
	// Through the hook Register installed, as the server's own Shutdown runs it.
	if err := v.e.Server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.next(t); ok {
		t.Errorf("the stream outlived the shutdown: %q", got)
	}
}

// One session holds a few streams, not an unbounded number; a closed stream
// frees its place.
func TestEventsAreLimitedPerSession(t *testing.T) {
	v, srv, alice, id, _ := eventsEnv(t)
	path := "/projects/" + id.String() + "/events"
	var open []*stream
	for i := 0; i < v.dash.maxStreams; i++ {
		s := v.openStream(t, srv, path, alice)
		s.next(t)
		open = append(open, s)
	}
	if s := v.openStream(t, srv, path, alice); s.resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("one stream too many: %d, want 429", s.resp.StatusCode)
	}
	// Another session is not affected.
	if s := v.openStream(t, srv, path, v.login(t, "alice@example.com", "alice-pass")); s.resp.StatusCode != 200 {
		t.Errorf("a second session: %d, want 200", s.resp.StatusCode)
	}
	open[0].cancel()
	deadline := time.Now().Add(3 * time.Second)
	for {
		v.dash.streamsMu.Lock()
		n := v.dash.streams[hashToken(alice.Value)]
		v.dash.streamsMu.Unlock()
		if n == v.dash.maxStreams-1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a closed stream still counts: %d open", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The page offers the stream only while something is in progress; offering it
// on a settled project would make the browser connect just to be told to stop.
func TestProjectPageOffersTheStreamOnlyWhileARolloutIsInProgress(t *testing.T) {
	v, _, alice, id, dep := eventsEnv(t)
	path := "/projects/" + id.String()
	if body := v.send("GET", path, nil, alice, nil).body; !strings.Contains(body, `data-events="`+path+`/events"`) {
		t.Error("a deploying project does not offer the stream")
	}
	v.settle(t, dep, project.StatusReady, "")
	if body := v.send("GET", path, nil, alice, nil).body; strings.Contains(body, "data-events") {
		t.Error("a settled project offers a stream that would end at once")
	}
}

func TestScriptIsServedWithItsOwnTypeAndVersion(t *testing.T) {
	v := newEnv(t)
	r := v.send("GET", "/assets/app.js?v="+v.dash.versions["app.js"], nil, nil, nil)
	if r.code != 200 || r.header.Get("Content-Type") != "text/javascript; charset=utf-8" || r.header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("versioned script: %d %q %q", r.code, r.header.Get("Content-Type"), r.header.Get("Cache-Control"))
	}
	if cc := v.send("GET", "/assets/app.js?v=stale", nil, nil, nil).header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("a stale script version must be revalidated, got %q", cc)
	}
	if !strings.Contains(v.send("GET", "/login", nil, nil, nil).body, `src="/assets/app.js?v=`+v.dash.versions["app.js"]+`"`) {
		t.Error("pages do not load the script by its versioned URL")
	}
}
