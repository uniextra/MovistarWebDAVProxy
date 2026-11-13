package main

import (
	"testing"
	"time"
)

func TestTrashEmptyInterval(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 0, "0": 0, "2h": 2 * time.Hour, "30s": 0, "basura": 0} {
		t.Setenv("WEB_TRASH_EMPTY_EVERY", v)
		if got := trashEmptyInterval(); got != want {
			t.Errorf("%q: %v, esperado %v", v, got, want)
		}
	}
}
