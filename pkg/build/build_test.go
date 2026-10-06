package build

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func goodSpec() Spec {
	return Spec{
		ID: uuid.NewString(), Repo: "octo/web", CommitSHA: strings.Repeat("a", 40), Context: "frontend", Dockerfile: "Dockerfile",
		PushRef:  "registry.registry.svc.cluster.local:5000/web/web:aaaaaaaaaaaa",
		CacheRef: "registry.registry.svc.cluster.local:5000/web/web:buildcache",
		Token:    "token",
	}
}

func TestAGoodSpecIsValid(t *testing.T) {
	if err := goodSpec().Validate(); err != nil {
		t.Fatal(err)
	}
}

// Every field ends up in a build's arguments. A comma in a reference would add an
// option to the output (push=false, or another registry), and a path with ".."
// would leave the repository.
func TestASpecThatCouldDoMoreThanBuildIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(*Spec){
		"an id that is not a uuid":      func(s *Spec) { s.ID = "build-1; rm -rf" },
		"a repository with a space":     func(s *Spec) { s.Repo = "octo/web repo" },
		"a repository with a url":       func(s *Spec) { s.Repo = "evil.com/x" + strings.Repeat("/y", 1) + "/z" },
		"a short commit":                func(s *Spec) { s.CommitSHA = "abc" },
		"a commit with a fragment":      func(s *Spec) { s.CommitSHA = strings.Repeat("a", 40) + ":../x" },
		"a context that leaves":         func(s *Spec) { s.Context = "../other" },
		"an absolute context":           func(s *Spec) { s.Context = "/etc" },
		"a context with a colon":        func(s *Spec) { s.Context = "a:b" },
		"a context with a comma":        func(s *Spec) { s.Context = "a,b" },
		"a dockerfile that leaves":      func(s *Spec) { s.Dockerfile = "../../Dockerfile" },
		"an empty dockerfile":           func(s *Spec) { s.Dockerfile = "" },
		"a push ref with a comma":       func(s *Spec) { s.PushRef = "reg:5000/web/web:1,push=false" },
		"a tag with only a comma":       func(s *Spec) { s.PushRef = "reg:5000/web/web:1,2" },
		"a path with only a comma":      func(s *Spec) { s.PushRef = "reg:5000/web,x/web:1" },
		"a host with only a comma":      func(s *Spec) { s.PushRef = "reg,x:5000/web/web:1" },
		"a cache tag with only a comma": func(s *Spec) { s.CacheRef = "reg:5000/web/web:cache,x" },
		"a push ref with a space":       func(s *Spec) { s.PushRef = "reg:5000/web/web:1 --foo" },
		"a push ref without a tag":      func(s *Spec) { s.PushRef = "reg:5000/web/web" },
		"a cache ref with a comma":      func(s *Spec) { s.CacheRef = "reg:5000/web/web:buildcache,mode=max" },
		"an uppercase push ref":         func(s *Spec) { s.PushRef = "reg:5000/Web/web:1" },
		"no token":                      func(s *Spec) { s.Token = "" },
		"a push ref with a double dash": func(s *Spec) { s.PushRef = "--output=x/web:1" },
	} {
		t.Run(name, func(t *testing.T) {
			s := goodSpec()
			mutate(&s)
			if err := s.Validate(); err == nil {
				t.Errorf("accepted %+v", s)
			}
		})
	}
}

func TestRedactRemovesWhatLooksLikeAToken(t *testing.T) {
	for in, want := range map[string]string{
		"cloning with ghs_16C7e42F292c6912E7710c838347Ae178B4a now":                   "cloning with *** now",
		"token=ghu_abcdefghijklmnopqrstuvwxyz0123456789 end":                          "token=*** end",
		"github_pat_11ABCDEFG0abcdefghijklmnop_0123456789abcdefghij":                  "***",
		"fetching https://x-access-token:SECRETVALUE123@github.com/o/r.git failed":    "fetching https://x-access-token:***@github.com/o/r.git failed",
		"nothing to hide here, and ghs_short is not a token":                          "nothing to hide here, and ghs_short is not a token",
		"two ghs_aaaaaaaaaaaaaaaaaaaaaaaa and ghr_bbbbbbbbbbbbbbbbbbbbbbbb in a line": "two *** and *** in a line",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHTTPRegistryDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodHead:
			t.Errorf("method %s", r.Method)
		case r.URL.Path == "/v2/web/web/manifests/aaa":
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
				t.Errorf("Accept = %q", r.Header.Get("Accept"))
			}
			w.Header().Set("Docker-Content-Digest", digest)
		case r.URL.Path == "/v2/web/web/manifests/nodigest":
		case r.URL.Path == "/v2/web/web/manifests/broken":
			w.Header().Set("Docker-Content-Digest", "md5:abc")
		case r.URL.Path == "/v2/web/web/manifests/down":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	r := &HTTPRegistry{BaseURL: srv.URL + "/"}

	if got, err := r.Digest(t.Context(), "web/web", "aaa"); err != nil || got != digest {
		t.Errorf("Digest = %q, %v", got, err)
	}
	if _, err := r.Digest(t.Context(), "web/web", "missing"); err != ErrNoImage {
		t.Errorf("a missing image: %v, want ErrNoImage", err)
	}
	for _, tag := range []string{"nodigest", "broken", "down"} {
		if got, err := r.Digest(t.Context(), "web/web", tag); err == nil || err == ErrNoImage {
			t.Errorf("%s: %q, %v; want an error that is not ErrNoImage, so the build is retried", tag, got, err)
		}
	}
}
