package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type fakeDoer struct {
	calls []string
}

func (f *fakeDoer) Post(url, _ string, body io.Reader) (*http.Response, error) {
	raw, _ := io.ReadAll(body)
	f.calls = append(f.calls, url+" "+string(raw))
	return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader("{}"))}, nil
}

func newTestWatcher(t *testing.T) (*watcher, *fakeDoer) {
	t.Helper()
	doer := &fakeDoer{}
	return &watcher{
		cfg:    &Config{AwfURL: "http://awf", MaxPerRun: 3},
		state:  newState(t.TempDir() + "/seen.jsonl"),
		client: doer,
	}, doer
}

func TestFirstSightFiresOnlyOpened(t *testing.T) {
	w, doer := newTestWatcher(t)
	rc := RepoConfig{Repo: "o/r", Workflow: "review-pr", Scope: "local", On: allEvents}
	p := pr{Number: 5, HeadRefOid: "sha1"}

	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}
	if len(doer.calls) != 1 || !strings.Contains(doer.calls[0], EventOpened) {
		t.Fatalf("first sight dispatched %v, want one pr-opened", doer.calls)
	}

	// Same commit, next pass: nothing new.
	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}
	if len(doer.calls) != 1 {
		t.Fatalf("unchanged PR dispatched again: %v", doer.calls[1:])
	}

	// New push: exactly one pr-updated, no second pr-opened.
	p.HeadRefOid = "sha2"
	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}
	if len(doer.calls) != 2 || !strings.Contains(doer.calls[1], EventUpdated) {
		t.Fatalf("new commit dispatched %v, want one pr-updated", doer.calls[1:])
	}
}

func TestMaxPerRunStopsThePass(t *testing.T) {
	w, doer := newTestWatcher(t)
	w.cfg.MaxPerRun = 1
	rc := RepoConfig{Repo: "o/r", Workflow: "review-pr", On: []string{EventOpened}}

	stop, err := w.handle(rc, pr{Number: 1, HeadRefOid: "a"})
	if err != nil || stop {
		t.Fatalf("first PR: stop=%v err=%v", stop, err)
	}
	stop, err = w.handle(rc, pr{Number: 2, HeadRefOid: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !stop {
		t.Fatal("cap not reported after one dispatch")
	}
	if len(doer.calls) != 1 {
		t.Fatalf("dispatched %d, want 1", len(doer.calls))
	}
}

func TestFailedDispatchIsRetried(t *testing.T) {
	w, _ := newTestWatcher(t)
	w.client = errorDoer{}
	rc := RepoConfig{Repo: "o/r", Workflow: "review-pr", On: []string{EventOpened}}
	p := pr{Number: 9, HeadRefOid: "c"}

	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}
	if w.state.has(eventKey("o/r", 9, EventOpened, p.HeadRefOid)) {
		t.Fatal("failed dispatch was recorded, the retry is lost")
	}
}

type errorDoer struct{}

func (errorDoer) Post(string, string, io.Reader) (*http.Response, error) {
	return nil, io.ErrUnexpectedEOF
}

func TestReviewCommentFiresOncePerComment(t *testing.T) {
	w, doer := newTestWatcher(t)
	rc := RepoConfig{Repo: "o/r", Workflow: "reply-pr", On: []string{EventReviewComment}, OnlyAuthors: []string{"me"}}
	me := author{Login: "me"}

	if !rc.allows("me") || rc.allows("someone") {
		t.Fatal("only_authors filter is not honest")
	}

	p := pr{Number: 7, HeadRefOid: "sha1", Author: me,
		Comments: []comment{{ID: "c1", Author: me}}}
	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}
	if len(doer.calls) != 0 {
		t.Fatalf("own comment fired: %v", doer.calls)
	}

	p.Comments = append(p.Comments, comment{ID: "c2", Author: author{Login: "reviewer"}})
	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}
	if len(doer.calls) != 1 || !strings.Contains(doer.calls[0], EventReviewComment) {
		t.Fatalf("reviewer comment dispatched %v, want one review-comment", doer.calls)
	}

	// Same comment on the next pass: deduplicated by comment id.
	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}
	if len(doer.calls) != 1 {
		t.Fatalf("same comment dispatched twice: %v", doer.calls[1:])
	}

	// Our reply ends the streak, so nothing fires until the reviewer writes again.
	p.Comments = append(p.Comments, comment{ID: "c3", Author: me})
	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}
	if len(doer.calls) != 1 {
		t.Fatalf("own reply dispatched: %v", doer.calls[1:])
	}
}
