package secrets

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func newBox(t *testing.T, fill byte) *Box {
	t.Helper()
	b, err := New(bytes.Repeat([]byte{fill}, keySize))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The point of the box: what reaches the database must not contain the value.
func TestSealedValueDoesNotContainThePlaintext(t *testing.T) {
	const secret = "postgres://user:hunter2@db/app"
	sealed, err := newBox(t, 1).Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(secret)) || bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatal("the sealed bytes contain the plaintext")
	}
}

func TestRoundTrip(t *testing.T) {
	b := newBox(t, 1)
	for _, v := range []string{"", "x", strings.Repeat("long value ", 1000), "ünïcode ✓"} {
		sealed, err := b.Seal(v)
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.Open(sealed)
		if err != nil || got != v {
			t.Errorf("round trip of %q: got %q, %v", v, got, err)
		}
	}
}

// A fixed nonce would let equal values be recognised in a dump.
func TestSealingTwiceGivesDifferentBytes(t *testing.T) {
	b := newBox(t, 1)
	x, _ := b.Seal("same")
	y, _ := b.Seal("same")
	if bytes.Equal(x, y) {
		t.Fatal("two seals of one value are identical")
	}
}

func TestOpenFailsWithAnotherKeyOrAlteredData(t *testing.T) {
	sealed, _ := newBox(t, 1).Seal("value")
	if _, err := newBox(t, 2).Open(sealed); err == nil {
		t.Error("opened with the wrong key")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := newBox(t, 1).Open(sealed); err == nil {
		t.Error("opened altered data")
	}
	if _, err := newBox(t, 1).Open([]byte("short")); err == nil {
		t.Error("opened data shorter than a nonce")
	}
}

func TestParseKey(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, keySize))
	if _, err := ParseKey(good); err != nil {
		t.Errorf("valid key rejected: %v", err)
	}
	for name, bad := range map[string]string{
		"empty":     "",
		"not b64":   "%%%",
		"too short": base64.StdEncoding.EncodeToString([]byte("short")),
	} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
