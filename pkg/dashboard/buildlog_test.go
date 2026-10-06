package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/project"
)

// logEnv is an environment with a server on a real socket, a fast poll, and
// alice's repository project with one deployment whose build is running.
func logEnv(t *testing.T) (v *env, srv *httptest.Server, alice *http.Cookie, p *project.Project, dep uuid.UUID) {
	t.Helper()
	v, _ = newRepoEnv(t)
	v.dash.pollEvery = 10 * time.Millisecond
	srv = httptest.NewServer(v.e)
	t.Cleanup(srv.Close)
	alice = v.login(t, "alice@example.com", "alice-pass")
	p, err := v.projects.CreateProject(t.Context(), aliceID, project.CreateInput{Name: "Web", Slug: "web", Region: "eu", Repo: "octo/web", GitHubInstallationID: 1, GitHubRepoID: 10})
	if err != nil {
		t.Fatal(err)
	}
	q, err := v.projects.QueueFromPush(t.Context(), project.PushInput{InstallationID: 1, RepoID: 10, Ref: "refs/heads/main", CommitSHA: strings.Repeat("a", 40)})
	if err != nil || len(q) != 1 {
		t.Fatal(err)
	}
	dep = q[0].Deployment.ID
	if err := v.projects.StartBuild(t.Context(), dep); err != nil {
		t.Fatal(err)
	}
	return v, srv, alice, p, dep
}

func (v *env) moveTo(t *testing.T, dep uuid.UUID, from, to project.Status, reason string) {
	t.Helper()
	if err := v.store.Move(context.Background(), dep, from, to, reason); err != nil {
		t.Fatal(err)
	}
}

func (v *env) record(t *testing.T, dep uuid.UUID, text string) {
	t.Helper()
	if err := v.projects.RecordBuildLog(context.Background(), dep, text); err != nil {
		t.Fatal(err)
	}
}

func logPath(p *project.Project, dep uuid.UUID, tail string) string {
	return "/projects/" + p.ID.String() + "/deployments/" + dep.String() + tail
}

// frameOf decodes the data of a "log" event as the page's script does.
func frameOf(t *testing.T, event string) logFrame {
	t.Helper()
	data, ok := strings.CutPrefix(event, "log ")
	if !ok {
		t.Fatalf("not a log event: %q", event)
	}
	var f logFrame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		t.Fatalf("%v: %q", err, data)
	}
	return f
}

// The first event carries the whole log, so a page that was rendered a moment
// earlier is corrected; after that only what was added arrives, and when the
// build stops printing the stream ends, with the status that ended it.
func TestLogStreamSendsTheLogThenWhatIsAddedThenEnds(t *testing.T) {
	v, srv, alice, p, dep := logEnv(t)
	v.record(t, dep, "#1 fetch\n")
	s := v.openStream(t, srv, logPath(p, dep, "/log/stream"), alice)
	if ct := s.resp.Header.Get("Content-Type"); s.resp.StatusCode != 200 || ct != "text/event-stream" {
		t.Fatalf("%d %q", s.resp.StatusCode, ct)
	}
	got, _ := s.next(t)
	if f := frameOf(t, got); f.Text != "#1 fetch\n" || !f.Reset || f.Status != project.StatusBuilding {
		t.Errorf("first event: %+v", f)
	}

	v.record(t, dep, "#1 fetch\n#2 build\n")
	got, _ = s.next(t)
	if f := frameOf(t, got); f.Text != "#2 build\n" || f.Reset {
		t.Errorf("the added text: %+v, want only what was added", f)
	}

	v.moveTo(t, dep, project.StatusBuilding, project.StatusFailed, "the build failed")
	got, _ = s.next(t)
	if f := frameOf(t, got); f.Status != project.StatusFailed || f.Error != "the build failed" || f.Text != "" {
		t.Errorf("the ending event: %+v", f)
	}
	if got, _ := s.next(t); got != "done {}" {
		t.Errorf("after the build ended: %q, want done", got)
	}
	if got, ok := s.next(t); ok {
		t.Errorf("the stream stayed open after done: %q", got)
	}
}

// What a Dockerfile prints is data. A line that looks like the end of an event
// and the start of another must reach the page as text inside one event.
func TestLogStreamKeepsWhatABuildPrintedInsideOneEvent(t *testing.T) {
	v, srv, alice, p, dep := logEnv(t)
	evil := "x\n\nevent: done\ndata: {}\n\nevent: log\ndata: {\"text\":\"forged\"}\n\n"
	v.record(t, dep, evil)
	s := v.openStream(t, srv, logPath(p, dep, "/log/stream"), alice)
	got, _ := s.next(t)
	if f := frameOf(t, got); f.Text != evil {
		t.Errorf("the log did not arrive as it was printed: %q", f.Text)
	}
	// Nothing else may have been read from it: the build is still running.
	select {
	case extra, ok := <-s.events:
		t.Errorf("the printed text produced another event (%v): %q", ok, extra)
	case <-time.After(100 * time.Millisecond):
	}
}

// The stored log is a tail. When it is full its beginning moves, and the stream
// must send what was printed, not the whole tail again.
func TestLogStreamSendsOnlyTheNewTextOnceTheTailHasSlid(t *testing.T) {
	v, srv, alice, p, dep := logEnv(t)
	old := uniqueLog(3 * logAnchor)
	v.record(t, dep, old)
	s := v.openStream(t, srv, logPath(p, dep, "/log/stream"), alice)
	s.next(t)

	slid := old[5000:] + "the new output\n"
	v.record(t, dep, slid)
	got, _ := s.next(t)
	if f := frameOf(t, got); f.Text != "the new output\n" || f.Reset {
		t.Errorf("after the tail slid: reset=%v, %d bytes (%.60q), want only the new output", f.Reset, len(f.Text), f.Text)
	}
}

func TestLogDelta(t *testing.T) {
	tests := []struct {
		name, prev, cur, text string
		reset                 bool
	}{
		{"nothing new", "abc", "abc", "", false},
		{"appended", "abc", "abcdef", "def", false},
		{"from nothing", "", "abc", "abc", false},
		{"nothing in common", "abc", "xyz", "xyz", true},
		{"the log was replaced by a shorter one", "abcdef", "ab", "ab", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if text, reset := logDelta(tc.prev, tc.cur); text != tc.text || reset != tc.reset {
				t.Errorf("logDelta(%q, %q) = %q, %v; want %q, %v", tc.prev, tc.cur, text, reset, tc.text, tc.reset)
			}
		})
	}
}

// uniqueLog is a log no part of which repeats, as a build's is.
func uniqueLog(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString("#" + uuid.NewString() + " step\n")
	}
	return b.String()
}

// Once the stored tail is full its beginning moves: what is left of the old text
// is the start of the new one, and what follows is what was printed.
func TestLogDeltaFindsWhereTheOldTextEndsInANewerTail(t *testing.T) {
	prev := uniqueLog(8 * logAnchor)
	for _, cut := range []int{1, 100, logAnchor, 3 * logAnchor} {
		if text, reset := logDelta(prev, prev[cut:]+"new\n"); text != "new\n" || reset {
			t.Errorf("cut %d: %d bytes, reset=%v, want %q", cut, len(text), reset, "new\n")
		}
	}
	// A tail that moved with nothing added since says nothing new.
	if text, reset := logDelta(prev, prev[500:]); text != "" || reset {
		t.Errorf("a slid tail with nothing added: %d bytes, reset=%v", len(text), reset)
	}
	// The end of the old text matching by chance is not enough: all that is left of
	// the old text must be the start of the new one.
	if text, reset := logDelta(prev, "unrelated\n"+prev[len(prev)-logAnchor:]+"after\n"); !reset || !strings.HasSuffix(text, "after\n") {
		t.Errorf("a coincidental match: reset=%v, %d bytes", reset, len(text))
	}
}

// A log of one repeated line cannot say where it ends. The viewer may be sent
// some of it again, but never misses any, and the search stays bounded.
func TestLogDeltaOnARepeatedLogResendsRatherThanDrops(t *testing.T) {
	prev := strings.Repeat("0123456789abcdef", 8*logAnchor/16)
	text, reset := logDelta(prev, prev[100:]+"tail")
	if reset || !strings.HasSuffix(text, "tail") {
		t.Errorf("reset=%v, %d bytes, want the new text at the end and no reset", reset, len(text))
	}
	if text, reset := logDelta(prev, strings.Repeat("0123456789abcdef", 8*logAnchor/16)+"x"); reset || text != "x" {
		t.Errorf("an appended repeat: reset=%v, %d bytes", reset, len(text))
	}
}

// A build that has moved past the point of printing has nothing more to stream,
// even though its deployment goes on.
func TestLogStreamEndsWhenTheBuildHasFinishedEvenIfTheDeploymentGoesOn(t *testing.T) {
	v, srv, alice, p, dep := logEnv(t)
	v.record(t, dep, "built\n")
	v.moveTo(t, dep, project.StatusBuilding, project.StatusPushing, "")
	v.moveTo(t, dep, project.StatusPushing, project.StatusDeploying, "")
	s := v.openStream(t, srv, logPath(p, dep, "/log/stream"), alice)
	got, _ := s.next(t)
	if f := frameOf(t, got); f.Text != "built\n" || f.Status != project.StatusDeploying {
		t.Errorf("first event: %+v", f)
	}
	if got, _ := s.next(t); got != "done {}" {
		t.Errorf("second event: %q, want done", got)
	}
}

// Pushing the image still prints (BuildKit reports the export), so the stream
// stays open until the build has really finished, and then ends on its own.
func TestLogStreamStaysOpenWhilePushingAndEndsWhenDeploying(t *testing.T) {
	v, srv, alice, p, dep := logEnv(t)
	v.moveTo(t, dep, project.StatusBuilding, project.StatusPushing, "")
	v.record(t, dep, "#12 pushing\n")
	s := v.openStream(t, srv, logPath(p, dep, "/log/stream"), alice)
	got, _ := s.next(t)
	if f := frameOf(t, got); f.Status != project.StatusPushing || f.Text != "#12 pushing\n" {
		t.Errorf("first event: %+v", f)
	}
	select {
	case extra, ok := <-s.events:
		t.Errorf("the stream ended or spoke while the image was being pushed (%v): %q", ok, extra)
	case <-time.After(100 * time.Millisecond):
	}
	v.record(t, dep, "#12 pushing\n#13 done\n")
	got, _ = s.next(t)
	if f := frameOf(t, got); f.Text != "#13 done\n" {
		t.Errorf("the last lines: %+v", f)
	}
	v.moveTo(t, dep, project.StatusPushing, project.StatusDeploying, "")
	s.next(t) // the status change
	if got, _ := s.next(t); got != "done {}" {
		t.Errorf("after the push: %q, want done", got)
	}
}

// A deployment that was queued behind another build has no log yet but may get
// one, so its stream stays open.
func TestLogStreamStaysOpenForAQueuedDeployment(t *testing.T) {
	v, srv, alice, p, _ := logEnv(t)
	q, err := v.projects.QueueFromPush(t.Context(), project.PushInput{InstallationID: 1, RepoID: 10, Ref: "refs/heads/main", CommitSHA: strings.Repeat("b", 40)})
	if err != nil || len(q) != 1 {
		t.Fatal(err)
	}
	s := v.openStream(t, srv, logPath(p, q[0].Deployment.ID, "/log/stream"), alice)
	got, _ := s.next(t)
	if f := frameOf(t, got); f.Status != project.StatusQueued || f.Text != "" {
		t.Errorf("first event: %+v", f)
	}
	select {
	case extra, ok := <-s.events:
		t.Errorf("a queued deployment's stream ended or spoke (%v): %q", ok, extra)
	case <-time.After(100 * time.Millisecond):
	}
}

// The ownership rule is the page's: another user's deployment, one under another
// project's path, an unknown one and a malformed id all answer 404, and no stream
// starts.
func TestLogStreamOfADeploymentThatIsNotTheCallersIs404(t *testing.T) {
	v, srv, alice, p, dep := logEnv(t)
	bob := v.login(t, "bob@example.com", "bob-pass")
	other, err := v.projects.CreateProject(t.Context(), aliceID, project.CreateInput{Name: "Other", Slug: "other", Region: "eu"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		who  *http.Cookie
		path string
	}{
		{bob, logPath(p, dep, "/log/stream")},
		{alice, logPath(other, dep, "/log/stream")},
		{alice, "/projects/" + p.ID.String() + "/deployments/" + uuid.NewString() + "/log/stream"},
		{alice, "/projects/" + p.ID.String() + "/deployments/nope/log/stream"},
		{alice, "/projects/nope/deployments/" + dep.String() + "/log/stream"},
	} {
		req, _ := http.NewRequest("GET", srv.URL+tc.path, nil)
		req.AddCookie(tc.who)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Content-Type") == "text/event-stream" {
			t.Errorf("%s: %d %q", tc.path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
}

func TestLogStreamNeedsASession(t *testing.T) {
	_, srv, _, p, dep := logEnv(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(srv.URL + logPath(p, dep, "/log/stream"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("anonymous: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// Signing out must cut off a log that is already streaming.
func TestLogStreamEndsWhenTheSessionEnds(t *testing.T) {
	v, srv, alice, p, dep := logEnv(t)
	s := v.openStream(t, srv, logPath(p, dep, "/log/stream"), alice)
	s.next(t)
	if err := v.sessions.Delete(context.Background(), hashToken(alice.Value)); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.next(t); ok {
		t.Errorf("the stream outlived its session: %q", got)
	}
}

// A log page and the project page are two of a session's few streams: the log
// stream is counted with the status stream, not beside it.
func TestLogStreamsCountAgainstTheSessionsLimit(t *testing.T) {
	v, srv, alice, p, dep := logEnv(t)
	for i := 0; i < v.dash.maxStreams-1; i++ {
		v.openStream(t, srv, logPath(p, dep, "/log/stream"), alice).next(t)
	}
	v.openStream(t, srv, "/projects/"+p.ID.String()+"/events", alice).next(t)
	if s := v.openStream(t, srv, logPath(p, dep, "/log/stream"), alice); s.resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("one stream too many: %d, want 429", s.resp.StatusCode)
	}
}

func TestLogStreamEndsAtItsMaximumAge(t *testing.T) {
	v, srv, alice, p, dep := logEnv(t)
	v.dash.maxStreamAge = 50 * time.Millisecond
	s := v.openStream(t, srv, logPath(p, dep, "/log/stream"), alice)
	s.next(t)
	if got, ok := s.next(t); ok {
		t.Errorf("a stream past its maximum age sent %q", got)
	}
}

// The page has the log in it, so it works without scripts, and what a build
// printed cannot become markup. The stream is offered only while the build runs.
func TestBuildLogPageShowsTheLogAndOffersTheStreamOnlyWhileBuilding(t *testing.T) {
	v, _, alice, p, dep := logEnv(t)
	v.record(t, dep, "#1 <script>alert(1)</script>\n")
	r := v.send("GET", logPath(p, dep, ""), nil, alice, nil)
	if r.code != http.StatusOK {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if strings.Contains(r.body, "<script>alert(1)</script>") || !strings.Contains(r.body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("the log was not escaped")
	}
	if !strings.Contains(r.body, `data-log-stream="`+logPath(p, dep, "/log/stream")+`"`) {
		t.Errorf("a running build's page does not offer the stream")
	}
	if !strings.Contains(r.body, `href="`+logPath(p, dep, "/log")+`"`) {
		t.Errorf("the page has no link to the raw log")
	}

	v.moveTo(t, dep, project.StatusBuilding, project.StatusFailed, "boom")
	r = v.send("GET", logPath(p, dep, ""), nil, alice, nil)
	if strings.Contains(r.body, "data-log-stream") || !strings.Contains(r.body, "boom") {
		t.Errorf("a finished build's page offers the stream, or hides why it failed")
	}
}

func TestBuildLogPageIsTheOwnersAlone(t *testing.T) {
	v, _, _, p, dep := logEnv(t)
	bob := v.login(t, "bob@example.com", "bob-pass")
	v.record(t, dep, "secret output\n")
	other, _ := v.projects.CreateProject(t.Context(), aliceID, project.CreateInput{Name: "Other", Slug: "other", Region: "eu"})
	alice := v.login(t, "alice@example.com", "alice-pass")
	for _, tc := range []struct {
		who  *http.Cookie
		path string
	}{{bob, logPath(p, dep, "")}, {alice, logPath(other, dep, "")}, {alice, "/projects/" + p.ID.String() + "/deployments/nope"}} {
		if r := v.send("GET", tc.path, nil, tc.who, nil); r.code != http.StatusNotFound || strings.Contains(r.body, "secret output") {
			t.Errorf("%s: %d", tc.path, r.code)
		}
	}
	if r := v.send("GET", logPath(p, dep, ""), nil, nil, nil); r.code != http.StatusSeeOther || r.header.Get("Location") != "/login" {
		t.Errorf("anonymous: %d %s", r.code, r.header.Get("Location"))
	}
}
