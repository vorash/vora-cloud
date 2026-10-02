package dashboard

import (
	"bytes"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vorash/vora-cloud/pkg/project"
)

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 32, 0, 0, time.UTC)
	cases := map[string]struct {
		ago  time.Duration
		want string
	}{
		"seconds":          {20 * time.Second, "just now"},
		"a clock ahead":    {-5 * time.Minute, "just now"},
		"one minute":       {time.Minute + 59*time.Second, "1 minute ago"},
		"minutes":          {20 * time.Minute, "20 minutes ago"},
		"one hour":         {time.Hour, "1 hour ago"},
		"hours":            {23*time.Hour + 59*time.Minute, "23 hours ago"},
		"one day":          {24 * time.Hour, "1 day ago"},
		"days":             {29 * 24 * time.Hour, "29 days ago"},
		"a month or more":  {30 * 24 * time.Hour, "31 Aug 2026"},
		"a year and a day": {366 * 24 * time.Hour, "29 Sep 2025"},
	}
	for name, c := range cases {
		if got := relTime(now, now.Add(-c.ago)); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

var cssVersionRe = regexp.MustCompile(`/assets/app\.css\?v=([0-9a-f]+)"`)

// The fonts and logo are served from the binary with explicit types (nosniff
// is on), the stylesheet is cached for good only under its own version, and
// nothing else in the embedded directory, such as the licence texts, is served.
func TestAssetsAreServedWithTheirTypeAndCache(t *testing.T) {
	v := newEnv(t)
	m := cssVersionRe.FindStringSubmatch(v.send("GET", "/login", nil, nil, nil).body)
	if m == nil {
		t.Fatal("the sign-in page does not link a versioned stylesheet")
	}

	css := v.send("GET", "/assets/app.css?v="+m[1], nil, nil, nil)
	if css.code != 200 || css.header.Get("Content-Type") != "text/css; charset=utf-8" || css.header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("versioned stylesheet: %d %q %q", css.code, css.header.Get("Content-Type"), css.header.Get("Cache-Control"))
	}
	if cc := v.send("GET", "/assets/app.css", nil, nil, nil).header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("an unversioned or stale stylesheet must be revalidated, got %q", cc)
	}
	if cc := v.send("GET", "/assets/app.css?v=stale", nil, nil, nil).header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("a stale version must be revalidated, got %q", cc)
	}

	font := v.send("GET", "/assets/fonts/switzer-variable.woff2", nil, nil, nil)
	if font.code != 200 || font.header.Get("Content-Type") != "font/woff2" || !strings.HasPrefix(font.body, "wOF2") {
		t.Errorf("font: %d %q", font.code, font.header.Get("Content-Type"))
	}
	if csp := font.header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("assets carry the security headers too: %q", csp)
	}
	logo := v.send("GET", "/assets/brand/vora-logo.svg", nil, nil, nil)
	if logo.code != 200 || logo.header.Get("Content-Type") != "image/svg+xml" || !strings.Contains(logo.body, "<svg") {
		t.Errorf("logo: %d %q", logo.code, logo.header.Get("Content-Type"))
	}

	for _, path := range []string{"/assets/fonts/FFL.txt", "/assets/fonts/OFL.txt", "/assets/../templates/base.html", "/assets/brand", "/assets/nope.css", "/assets/"} {
		if r := v.send("GET", path, nil, nil, nil); r.code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, r.code)
		}
	}
}

// An SVG opened directly is a document that can run script, so none of the
// embedded ones may carry any.
func TestEmbeddedSVGsHaveNoScript(t *testing.T) {
	err := fs.WalkDir(assetFS, "assets", func(name string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(name, ".svg") {
			return err
		}
		b, err := assetFS.ReadFile(name)
		if err != nil {
			return err
		}
		if s := strings.ToLower(string(b)); strings.Contains(s, "<script") || strings.Contains(s, "onload=") || strings.Contains(s, "javascript:") {
			t.Errorf("%s contains script", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

const wantPolicy = "default-src 'none'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; font-src 'self'; img-src 'self'; script-src 'self'; connect-src 'self'"

// ADR 004: the policy allows this origin's own fonts, images, script and
// requests (for the event stream), and nothing else: no inline script or style,
// no third party.
func TestPolicyAllowsOnlyOwnResources(t *testing.T) {
	v := newEnv(t)
	want := wantPolicy
	for _, path := range []string{"/login", "/assets/app.css"} {
		if got := v.send("GET", path, nil, nil, nil).header.Get("Content-Security-Policy"); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

var inlineRe = regexp.MustCompile(`(?i)<style|\sstyle=|\sonclick=|\sonload=|javascript:`)

// Every script tag must load the dashboard's own file and carry no code.
var scriptTagRe = regexp.MustCompile(`(?i)<script[^>]*>(?:[^<]*</script>)?`)
var ownScriptRe = regexp.MustCompile(`^<script src="/assets/app\.js\?v=[0-9a-f]+" defer></script>$`)

// The policy would block inline styles and scripts, so the markup must have none.
func TestPagesHaveNoInlineStylesOrScripts(t *testing.T) {
	v := newEnv(t)
	alice := v.login(t, "alice@example.com", "alice-pass")
	page := v.createProject(t, alice, "Shop", "shop")
	for _, path := range []string{"/login", "/", "/projects/new", page, "/nowhere"} {
		r := v.send("GET", path, nil, alice, map[string]string{"Accept": "text/html"})
		if m := inlineRe.FindString(r.body); m != "" {
			t.Errorf("%s has %q", path, m)
		}
		for _, tag := range scriptTagRe.FindAllString(r.body, -1) {
			if !ownScriptRe.MatchString(tag) {
				t.Errorf("%s has a script tag that is not the dashboard's own file: %q", path, tag)
			}
		}
	}
}

func TestSignInPageHasNoNavigation(t *testing.T) {
	v := newEnv(t)
	body := v.send("GET", "/login", nil, nil, nil).body
	if strings.Contains(body, `class="rail"`) || strings.Contains(body, `role="alert"`) || !strings.Contains(body, `class="auth-card"`) {
		t.Errorf("sign-in page: %s", body)
	}
	refused := v.send("POST", "/login", url.Values{"email": {"alice@example.com"}, "password": {"nope"}}, nil, nil)
	if !strings.Contains(refused.body, `role="alert"`) || !strings.Contains(refused.body, "Invalid email or password.") || strings.Contains(refused.body, `class="rail"`) {
		t.Errorf("refused sign-in: %d %s", refused.code, refused.body)
	}
}

func TestSignedInPagesHaveTheRailAndSignOut(t *testing.T) {
	v := newEnv(t)
	alice := v.login(t, "alice@example.com", "alice-pass")
	home := v.send("GET", "/", nil, alice, nil).body
	for _, want := range []string{`class="rail"`, "Alice", `action="/logout"`, `aria-current="page"`, `href="/projects/new"`} {
		if !strings.Contains(home, want) {
			t.Errorf("projects page lacks %q", want)
		}
	}
	if sub := v.send("GET", "/projects/new", nil, alice, nil).body; strings.Contains(sub, `aria-current="page"`) || !strings.Contains(sub, `class="is-here"`) {
		t.Error("a page below the list marks the section, not the page, as current")
	}
}

func TestProjectsPageShowsTheLatestDeploymentOfEachProject(t *testing.T) {
	v := newEnv(t)
	alice := v.login(t, "alice@example.com", "alice-pass")
	if body := v.send("GET", "/", nil, alice, nil).body; !strings.Contains(body, "Create your first project") {
		t.Error("an empty list must offer the next step")
	}
	shop := v.createProject(t, alice, "Shop", "shop")
	v.createProject(t, alice, "Idle", "idle")
	tok := v.csrf(t, alice)
	if r := v.send("POST", shop+"/deployments", url.Values{"csrf": {tok}, "image": {"reg/shop:1"}}, alice, nil); r.code != http.StatusSeeOther {
		t.Fatalf("deploy: %d", r.code)
	}

	body := v.send("GET", "/", nil, alice, nil).body
	for _, want := range []string{"status-deploying", `href="https://shop.eu.cloud.example.com"`, ">shop.eu.cloud.example.com<", "No deployments", `<time datetime="`, " UTC"} {
		if !strings.Contains(body, want) {
			t.Errorf("projects page lacks %q", want)
		}
	}
	bob := v.login(t, "bob@example.com", "bob-pass")
	if body := v.send("GET", "/", nil, bob, nil).body; strings.Contains(body, "shop") || strings.Contains(body, "status-deploying") {
		t.Error("another user's project or deployment showed on bob's page")
	}
}

func TestProjectPageLinksTheAddressAndOffersTheNextStep(t *testing.T) {
	v := newEnv(t)
	alice := v.login(t, "alice@example.com", "alice-pass")
	shop := v.createProject(t, alice, "Shop", "shop")

	empty := v.send("GET", shop, nil, alice, nil).body
	if !strings.Contains(empty, "No address until you deploy.") || !strings.Contains(empty, "No deployments yet") || strings.Contains(empty, "Redeploy") {
		t.Error("a project without deployments has no address and no redeploy, and says what to do")
	}

	tok := v.csrf(t, alice)
	r := v.send("POST", shop+"/deployments", url.Values{"csrf": {tok}, "image": {"reg/shop:1"}}, alice, nil)
	row := strings.TrimPrefix(r.header.Get("Location"), shop+"#")
	if !strings.HasPrefix(row, "deployment-") {
		t.Fatalf("a deploy should land on the new row, got %q", r.header.Get("Location"))
	}
	body := v.send("GET", shop, nil, alice, nil).body
	for _, want := range []string{`id="` + row + `"`, `<a class="mono" href="https://shop.eu.cloud.example.com"`, "reg/shop:1", "Redeploy latest image"} {
		if !strings.Contains(body, want) {
			t.Errorf("project page lacks %q", want)
		}
	}
}

// A failed deployment shows its reason in the row, escaped, and the times are
// relative to the clock with the exact UTC time in the title.
func TestProjectPageShowsWhyADeploymentFailed(t *testing.T) {
	v := newEnv(t)
	now := time.Date(2026, 9, 30, 14, 32, 0, 0, time.UTC)
	v.dash.now = func() time.Time { return now }
	var buf bytes.Buffer
	failed := project.Deployment{
		ID: uuid.New(), Status: project.StatusFailed, Image: "reg/shop:2", URL: "https://shop.eu.cloud.example.com",
		Error: "boom <b>bad</b>", CreatedAt: now.Add(-2 * time.Hour),
	}
	err := v.dash.pages["project"].ExecuteTemplate(&buf, "base", page{
		Title: "Shop", Shell: "app", User: "Alice", CSRF: "tok", Asset: "v1",
		Data: projectData{
			Project:     &project.Project{ID: uuid.New(), Name: "Shop", Slug: "shop", Region: "eu"},
			Deployments: []project.Deployment{failed},
			Latest:      &failed,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, want := range []string{
		`<details class="err"><summary>boom &lt;b&gt;bad&lt;/b&gt;</summary></details>`,
		"status-failed", ">2 hours ago</time>", `title="2026-09-30 12:32 UTC"`, `datetime="2026-09-30T12:32:00Z"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("project page lacks %q", want)
		}
	}
}

func TestErrorPagesShowTheirStatus(t *testing.T) {
	v := newEnv(t)
	alice := v.login(t, "alice@example.com", "alice-pass")
	missing := v.send("GET", "/projects/"+uuid.NewString(), nil, alice, nil)
	if missing.code != http.StatusNotFound || !strings.Contains(missing.body, "Page not found") || !strings.Contains(missing.body, ">404<") || !strings.Contains(missing.body, `class="rail"`) {
		t.Errorf("signed-in 404: %d %s", missing.code, missing.body)
	}
	if strings.Contains(missing.body, `role="alert"`) {
		t.Error("an error page is a page, not an alert above one")
	}
	refused := v.send("POST", "/login", url.Values{}, nil, map[string]string{"Origin": "https://evil.example"})
	if refused.code != http.StatusForbidden || !strings.Contains(refused.body, "Request refused") || strings.Contains(refused.body, `class="rail"`) || !strings.Contains(refused.body, "anon-bar") {
		t.Errorf("signed-out 403: %d %s", refused.code, refused.body)
	}
}

func TestNewProjectFormKeepsTheChoicesAfterARefusal(t *testing.T) {
	v := newEnv(t)
	alice := v.login(t, "alice@example.com", "alice-pass")
	v.createProject(t, alice, "Shop", "shop")
	bob := v.login(t, "bob@example.com", "bob-pass")

	fresh := v.send("GET", "/projects/new", nil, bob, nil).body
	if !strings.Contains(fresh, `name="region" value="eu" checked`) || strings.Contains(fresh, `aria-invalid`) {
		t.Errorf("a fresh form picks the first region and marks nothing: %s", fresh)
	}
	r := v.send("POST", "/projects", url.Values{"csrf": {v.csrf(t, bob)}, "name": {"Mine"}, "slug": {"shop"}, "region": {"eu"}}, bob, nil)
	for _, want := range []string{`role="alert"`, `aria-invalid="true"`, `value="Mine"`, `value="shop"`, `name="region" value="eu" checked`} {
		if r.code != http.StatusUnprocessableEntity || !strings.Contains(r.body, want) {
			t.Errorf("refused form (%d) lacks %q", r.code, want)
		}
	}
}

// A long history must not make the page grow: it lists the newest rows and
// links to the older ones, and the overview and Redeploy keep going by the
// newest deployment, not by the first row of whichever page is open.
func TestProjectPageListsTheNewestDeploymentsAndLinksToOlderOnes(t *testing.T) {
	v := newEnv(t)
	alice := v.login(t, "alice@example.com", "alice-pass")
	shop := v.createProject(t, alice, "Shop", "shop")
	tok := v.csrf(t, alice)
	total := deploymentsPerPage + 5
	for i := 1; i <= total; i++ {
		img := fmt.Sprintf("reg/shop:%d", i)
		if r := v.send("POST", shop+"/deployments", url.Values{"csrf": {tok}, "image": {img}}, alice, nil); r.code != http.StatusSeeOther {
			t.Fatalf("deploy %s: %d", img, r.code)
		}
	}
	image := func(i int) string { return fmt.Sprintf("<code>reg/shop:%d</code>", i) }

	first := v.send("GET", shop, nil, alice, nil).body
	if n := strings.Count(first, `class="tl-item"`); n != deploymentsPerPage {
		t.Errorf("first page lists %d deployments, want %d", n, deploymentsPerPage)
	}
	if !strings.Contains(first, image(total)) || strings.Contains(first, image(1)) {
		t.Error("the first page must hold the newest deployments and not the oldest")
	}
	m := regexp.MustCompile(`href="(` + regexp.QuoteMeta(shop) + `\?before=[^"]+)">Show older deployments`).FindStringSubmatch(first)
	if m == nil {
		t.Fatal("the first page has no link to the older deployments")
	}

	older := v.send("GET", m[1], nil, alice, nil)
	if older.code != http.StatusOK {
		t.Fatalf("older page: %d", older.code)
	}
	if n := strings.Count(older.body, `class="tl-item"`); n != total-deploymentsPerPage {
		t.Errorf("older page lists %d deployments, want %d", n, total-deploymentsPerPage)
	}
	if !strings.Contains(older.body, image(1)) || !strings.Contains(older.body, image(total-deploymentsPerPage)) || strings.Contains(older.body, image(total)) {
		t.Error("the older page must continue exactly where the first ended")
	}
	if strings.Contains(older.body, "Show older deployments") {
		t.Error("the last page must not offer older deployments")
	}
	if !strings.Contains(older.body, `<dd class="mono">reg/shop:`+fmt.Sprint(total)+`</dd>`) || !strings.Contains(older.body, "Redeploy latest image") {
		t.Error("on an older page the overview and Redeploy must still go by the newest deployment")
	}

	bob := v.login(t, "bob@example.com", "bob-pass")
	if r := v.send("GET", m[1], nil, bob, nil); r.code != http.StatusNotFound {
		t.Errorf("bob on alice's older page: %d, want 404", r.code)
	}
	if r := v.send("GET", shop+"?before=not-a-cursor", nil, alice, nil); r.code != http.StatusNotFound {
		t.Errorf("a made-up cursor: %d, want 404", r.code)
	}
}
