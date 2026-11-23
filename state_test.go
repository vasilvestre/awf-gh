package main

import (
	"os"
	"slices"
	"testing"
)

func TestEventKey(t *testing.T) {
	tests := []struct {
		name  string
		event string
		sha   string
		want  string
	}{
		{"opened ignores the sha", EventOpened, "aaa111", "o/r#12#pr-opened"},
		{"updated is keyed on the head", EventUpdated, "bbb222", "o/r#12#pr-updated@bbb222"},
		{"checks are keyed on the head", EventChecksFailed, "ccc333", "o/r#12#checks-failed@ccc333"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eventKey("o/r", 12, tt.event, tt.sha)
			if got != tt.want {
				t.Errorf("eventKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPRFailuresRetrigger(t *testing.T) {
	// The same PR pushed twice must yield two distinct update keys, otherwise
	// the second push never gets reviewed.
	p1 := pr{Number: 7, HeadRefOid: "sha1"}
	p2 := pr{Number: 7, HeadRefOid: "sha2"}
	k1 := eventKey("o/r", p1.Number, EventUpdated, p1.HeadRefOid)
	k2 := eventKey("o/r", p2.Number, EventUpdated, p2.HeadRefOid)
	if k1 == k2 {
		t.Fatalf("same key for different commits: %q", k1)
	}
}

func TestPREvents(t *testing.T) {
	red := pr{Number: 1, StatusCheckRollup: []check{{Conclusion: "FAILURE"}}}
	green := pr{Number: 2, StatusCheckRollup: []check{{Conclusion: "SUCCESS"}}}
	legacy := pr{Number: 3, StatusCheckRollup: []check{{State: "ERROR"}}}

	if got := red.events(allEvents); len(got) != 2 {
		t.Errorf("failing PR events = %v, want pr-opened and checks-failed only", got)
	}
	if got := red.events([]string{EventChecksFailed, EventUpdated}); len(got) != 1 || got[0] != EventChecksFailed {
		t.Errorf("failing PR events = %v, want checks-failed alone", got)
	}
	if green.failing() {
		t.Error("green PR reported as failing")
	}
	if legacy.events([]string{EventChecksFailed}) == nil {
		t.Error("legacy commit status ERROR not seen as failing")
	}
	if got := green.events([]string{EventOpened}); len(got) != 1 {
		t.Errorf("only pr-opened configured, got %v", got)
	}
}

func TestExecMapRoundTrip(t *testing.T) {
	path := t.TempDir() + "/seen.jsonl"
	st := newState(path)

	// No file yet: an empty map, not an error.
	execs, err := st.loadExec()
	if err != nil || len(execs) != 0 {
		t.Fatalf("loadExec on a fresh state = %v, %v; want empty", execs, err)
	}

	if err := st.add("o/r#3#seen"); err != nil {
		t.Fatal(err)
	}
	execs[execKey("o/r", 3, "review-pr", "review")] = []string{"exec-1"}
	if err := st.saveExec(execs); err != nil {
		t.Fatal(err)
	}

	reloaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.loadExec()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"exec-1"}; !slices.Equal(got["o/r#3#review-pr#review"], want) {
		t.Fatalf("reloaded %v, want o/r#3#review-pr#review = %v", got, want)
	}
	if reloaded.has("o/r#3") {
		t.Fatal("the exec map leaked into the append-only seen set")
	}
}

func TestExecWriteDoesNotFollowAPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	st := newState(dir + "/seen.jsonl")
	victim := dir + "/victim"
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, st.execPath()+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := st.saveExec(map[string][]string{"k": {"exec-1"}}); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(victim); err != nil || string(body) != "keep" {
		t.Fatalf("the write followed the symlink: %q, %v", body, err)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := t.TempDir() + "/seen.jsonl"
	st := newState(path)
	if err := st.add("o/r#1#pr-opened"); err != nil {
		t.Fatal(err)
	}
	if err := st.add("o/r#1#pr-opened"); err != nil {
		t.Fatal(err)
	}

	reloaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.has("o/r#1#pr-opened") {
		t.Fatal("key lost after reload")
	}
	if reloaded.has("o/r#2#pr-opened") {
		t.Fatal("unknown key reported as seen")
	}
	if len(reloaded.seen) != 1 {
		t.Fatalf("duplicate appended, got %d entries", len(reloaded.seen))
	}
}
