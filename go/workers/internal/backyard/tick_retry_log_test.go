package backyard

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTickRetryLogRateLimitsRepeatedErrors(t *testing.T) {
	var out bytes.Buffer
	l := tickRetryLog{out: &out}
	now := time.Unix(1_000, 0)
	a, b := errors.New("prepare: quote drift"), errors.New("custody hold")
	l.note(now, a)
	l.note(now.Add(10*time.Second), a)
	l.note(now.Add(20*time.Second), b)
	l.note(now.Add(30*time.Second), b)
	l.note(now.Add(90*time.Second), b)
	got := strings.Count(out.String(), "tick retried")
	if got != 3 || !strings.Contains(out.String(), "quote drift") || !strings.Contains(out.String(), "custody hold") {
		t.Fatalf("want 3 lines (first a, first b, b after a minute), got %d:\n%s", got, out.String())
	}
}
