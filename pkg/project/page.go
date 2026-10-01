package project

import (
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// DefaultPageSize is used when a caller sets no limit. It is above what a
	// project normally has, so callers written before paging keep working.
	DefaultPageSize = 50
	// MaxPageSize caps what one call returns.
	MaxPageSize = 100
)

// Cursor is a position in a project's deployments, newest first: the
// deployment a page ended on. The next page starts after it. created_at and id
// are the sort key, so a deployment created meanwhile cannot shift a page.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// String encodes the cursor as an opaque token, so callers do not depend on
// its parts. Microseconds are what the database stores.
func (c Cursor) String() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.CreatedAt.UnixMicro(), 10) + "." + c.ID.String()))
}

// ParseCursor decodes a token made by Cursor.String.
func ParseCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, invalid("before is not a cursor from this list")
	}
	micros, id, ok := strings.Cut(string(raw), ".")
	if !ok {
		return Cursor{}, invalid("before is not a cursor from this list")
	}
	us, err1 := strconv.ParseInt(micros, 10, 64)
	uid, err2 := uuid.Parse(id)
	if err1 != nil || err2 != nil {
		return Cursor{}, invalid("before is not a cursor from this list")
	}
	return Cursor{CreatedAt: time.UnixMicro(us), ID: uid}, nil
}

// Page asks for a window of a project's deployments.
type Page struct {
	// Limit is the most to return: 0 means DefaultPageSize, and more than
	// MaxPageSize is cut to it.
	Limit int
	// Before is the Next of an earlier page; empty starts at the newest.
	Before string
}

// DeploymentPage is a window of deployments, newest first.
type DeploymentPage struct {
	Deployments []Deployment
	// Next continues after this page, empty when it is the last.
	Next string
}
