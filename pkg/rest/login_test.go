package rest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/vorash/vora-cloud/pkg/auth"
	"github.com/vorash/vora-cloud/pkg/database"
)

type fakeLogins struct {
	calls int
	err   error
}

func (f *fakeLogins) LoginUser(context.Context, string, string) (string, string, *database.User, error) {
	f.calls++
	return "", "", nil, f.err
}

func loginAPI(f *fakeLogins, lim LoginLimiter) *echo.Echo {
	s := &Server{logins: f, opts: Options{Limiter: lim}}
	e := echo.New()
	e.POST("/login", s.login)
	return e
}

const loginBody = `{"email":"alice@example.com","password":"pw"}`

// The API must not say why a sign-in failed, and must not show an internal
// error to someone who has not signed in.
func TestLoginAnswersBadCredentialsWithoutDetail(t *testing.T) {
	f := &fakeLogins{err: auth.ErrInvalidCredentials}
	e := loginAPI(f, auth.NewAttemptLimiter(time.Minute, 5))
	code, body := call(t, e, "", "POST", "/login", loginBody)
	if code != http.StatusUnauthorized || body["error"] != "invalid email or password" {
		t.Errorf("got %d %v", code, body)
	}

	f.err = errors.New("failed to get user: dial tcp 10.0.0.5:5432: connection refused")
	code, body = call(t, e, "", "POST", "/login", `{"email":"bob@example.com","password":"pw"}`)
	if code != http.StatusInternalServerError || body["error"] != "internal error" {
		t.Errorf("internal failure: %d %v", code, body)
	}
}

// Guesses over REST are limited per account, and spend the same budget the
// dashboard's sign-in uses, so switching endpoints buys no new guesses.
func TestLoginIsLimitedPerAccountOnASharedBudget(t *testing.T) {
	f := &fakeLogins{err: auth.ErrInvalidCredentials}
	lim := auth.NewAttemptLimiter(time.Hour, 5)
	e := loginAPI(f, lim)

	var last int
	for i := 0; i < 8; i++ {
		last, _ = call(t, e, "", "POST", "/login", `{"email":"Alice@Example.com","password":"pw"}`)
	}
	if last != http.StatusTooManyRequests || f.calls != 5 {
		t.Errorf("after 8 guesses: %d, %d checked; want 429 and 5", last, f.calls)
	}
	if lim.Allow("alice@example.com") {
		t.Error("the dashboard's limiter still has budget for an account the API just exhausted")
	}
	if code, _ := call(t, e, "", "POST", "/login", `{"email":"bob@example.com","password":"pw"}`); code != http.StatusUnauthorized {
		t.Errorf("another account: %d, want a normal 401", code)
	}
}

func TestLoginRequiresBothFields(t *testing.T) {
	e := loginAPI(&fakeLogins{}, auth.NewAttemptLimiter(time.Minute, 5))
	for _, body := range []string{`{}`, `{"email":"a@example.com"}`, `{"password":"x"}`} {
		if code, _ := call(t, e, "", "POST", "/login", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", strings.TrimSpace(body), code)
		}
	}
}
