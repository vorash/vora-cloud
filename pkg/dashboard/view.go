package dashboard

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

// funcs are the helpers the templates use. Times are rendered here so the pages
// work without the script: "3 hours ago" is as of the request (the script then
// keeps it current), and the exact time goes in a title, always in UTC.
func (d *Dashboard) funcs() template.FuncMap {
	return template.FuncMap{
		"ago":   func(t time.Time) string { return relTime(d.now(), t) },
		"exact": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
		"iso":   func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		// host shows a deployment's URL without its scheme.
		"host": func(u string) string { return strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://") },
	}
}

// relTime says how long ago t was, in the largest whole unit; beyond a month it
// gives the date.
func relTime(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return ago(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return ago(int(d/time.Hour), "hour")
	case d < 30*24*time.Hour:
		return ago(int(d/(24*time.Hour)), "day")
	}
	return t.UTC().Format("2 Jan 2006")
}

func ago(n int, unit string) string {
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}

// statusTitle is the heading of an error page.
func statusTitle(status int) string {
	switch status {
	case http.StatusNotFound:
		return "Page not found"
	case http.StatusForbidden:
		return "Request refused"
	case http.StatusInternalServerError:
		return "Something went wrong"
	}
	return "Error"
}
