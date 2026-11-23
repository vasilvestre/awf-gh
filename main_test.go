package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type fakeDoer struct {
	calls    []string
	deleted  []string
	trail    []string
	nextID   int
	status   int
	failPost bool
	emptyID  bool
}

func (f *fakeDoer) Post(url, _ string, body io.Reader) (*http.Response, error) {
	raw, _ := io.ReadAll(body)
	f.calls = append(f.calls, url+" "+string(raw))
	f.trail = append(f.trail, "POST "+url)
	if f.failPost {
		return nil, io.ErrUnexpectedEOF
	}
	if f.emptyID {
		return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}
	f.nextID++
	reply := fmt.Sprintf(`{"body":{"execution_id":"exec-%d","status":"accepted"}}`, f.nextID)
	return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(reply))}, nil
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodDelete {
		return nil, fmt.Errorf("unexpected %s %s", req.Method, req.URL)
	}
	f.deleted = append(f.deleted, req.URL.Path)
	f.trail = append(f.trail, "DELETE "+req.URL.Path)
	status := f.status
	if status == 0 {
		status = http.StatusNoContent
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
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

func (errorDoer) Do(*http.Request) (*http.Response, error) {
	return nil, io.ErrUnexpectedEOF
}

func TestMovedHeadCancelsTheObsoleteRun(t *testing.T) {
	w, doer := newTestWatcher(t)
	rc := RepoConfig{Repo: "o/r", Workflow: "review-pr", On: []string{EventUpdated}}

	p := pr{Number: 4, HeadRefOid: "sha1"}
	if _, err := w.handle(rc, p); err != nil { // first sight: recorded, not dispatched
		t.Fatal(err)
	}
	p.HeadRefOid = "sha2"
	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}
	p.HeadRefOid = "sha3"
	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}

	if want := []string{"/api/executions/exec-1"}; !slices.Equal(doer.deleted, want) {
		t.Fatalf("deleted %v, want the first execution %v", doer.deleted, want)
	}
	if len(doer.trail) != 3 || doer.trail[2] != "DELETE /api/executions/exec-1" {
		t.Fatalf("trail = %v, want the DELETE after the second POST", doer.trail)
	}

	// Another pull request is a different run, not a superseded one.
	if _, err := w.handle(rc, pr{Number: 5, HeadRefOid: "sha4"}); err != nil {
		t.Fatal(err)
	}
	if len(doer.deleted) != 1 {
		t.Fatalf("another PR cancelled a run: %v", doer.deleted)
	}
}

func TestReviewCommentCancelsOnTheSameHead(t *testing.T) {
	w, doer := newTestWatcher(t)
	rc := RepoConfig{Repo: "o/r", Workflow: "reply-pr", On: []string{EventReviewComment}}
	me := author{Login: "me"}
	them := author{Login: "reviewer"}
	p := pr{Number: 6, HeadRefOid: "sha", Author: me,
		Comments: []comment{{ID: "c0", Author: me}}}

	if _, err := w.handle(rc, p); err != nil { // first sight: recorded, not dispatched
		t.Fatal(err)
	}
	p.Comments = append(p.Comments, comment{ID: "c1", Author: them})
	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}
	p.Comments = append(p.Comments, comment{ID: "c2", Author: them})
	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}

	if want := []string{"/api/executions/exec-1"}; !slices.Equal(doer.deleted, want) {
		t.Fatalf("deleted %v, want the run for the previous comment", doer.deleted)
	}
	if len(doer.calls) != 2 {
		t.Fatalf("dispatched %d, want both comments", len(doer.calls))
	}
}

func TestCancelFailuresKeepThePassGoing(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			w, doer := newTestWatcher(t)
			doer.status = status
			rc := RepoConfig{Repo: "o/r", Workflow: "review-pr", On: []string{EventUpdated}}

			if _, err := w.handle(rc, pr{Number: 8, HeadRefOid: "a"}); err != nil {
				t.Fatal(err)
			}
			if _, err := w.handle(rc, pr{Number: 8, HeadRefOid: "b"}); err != nil {
				t.Fatal(err)
			}
			stop, err := w.handle(rc, pr{Number: 8, HeadRefOid: "c"})
			if err != nil || stop {
				t.Fatalf("cancel answered %d: stop=%v err=%v", status, stop, err)
			}
			if len(doer.calls) != 2 || len(doer.deleted) != 1 {
				t.Fatalf("calls=%v deleted=%v, want both POSTs after the cancel",
					doer.calls, doer.deleted)
			}
		})
	}
}

func TestFailedCancelKeepsThePreviousEntry(t *testing.T) {
	w, doer := newTestWatcher(t)
	doer.status = http.StatusInternalServerError
	rc := RepoConfig{Repo: "o/r", Workflow: "review-pr", On: []string{EventUpdated}}

	if _, err := w.handle(rc, pr{Number: 17, HeadRefOid: "a"}); err != nil { // first sight
		t.Fatal(err)
	}
	if _, err := w.handle(rc, pr{Number: 17, HeadRefOid: "b"}); err != nil { // exec-1
		t.Fatal(err)
	}
	if _, err := w.handle(rc, pr{Number: 17, HeadRefOid: "c"}); err != nil { // the cancel fails
		t.Fatal(err)
	}
	doer.status = 0
	if _, err := w.handle(rc, pr{Number: 17, HeadRefOid: "d"}); err != nil { // retries the same run
		t.Fatal(err)
	}
	want := []string{"/api/executions/exec-1", "/api/executions/exec-1", "/api/executions/exec-2"}
	if !slices.Equal(doer.deleted, want) {
		t.Fatalf("deleted %v, want the failed run retried, then both superseded runs %v", doer.deleted, want)
	}
}

func TestFailedDispatchKeepsThePreviousRun(t *testing.T) {
	w, doer := newTestWatcher(t)
	rc := RepoConfig{Repo: "o/r", Workflow: "review-pr", On: []string{EventUpdated}}

	if _, err := w.handle(rc, pr{Number: 10, HeadRefOid: "a"}); err != nil { // first sight: recorded
		t.Fatal(err)
	}
	if _, err := w.handle(rc, pr{Number: 10, HeadRefOid: "b"}); err != nil {
		t.Fatal(err)
	}
	doer.failPost = true
	if _, err := w.handle(rc, pr{Number: 10, HeadRefOid: "c"}); err != nil {
		t.Fatal(err)
	}
	if len(doer.deleted) != 0 {
		t.Fatalf("a failed dispatch cancelled the live run: %v", doer.deleted)
	}
}

func TestChecksFailedCancelsTheSameHeadRun(t *testing.T) {
	w, doer := newTestWatcher(t)
	rc := RepoConfig{Repo: "o/r", Workflow: "review-pr", On: []string{EventUpdated, EventChecksFailed}}

	if _, err := w.handle(rc, pr{Number: 11, HeadRefOid: "a"}); err != nil { // first sight: recorded
		t.Fatal(err)
	}
	if _, err := w.handle(rc, pr{Number: 11, HeadRefOid: "b"}); err != nil {
		t.Fatal(err)
	}
	red := pr{Number: 11, HeadRefOid: "b", StatusCheckRollup: []check{{Conclusion: "FAILURE"}}}
	if _, err := w.handle(rc, red); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/api/executions/exec-1"}; !slices.Equal(doer.deleted, want) {
		t.Fatalf("deleted %v, want the pr-updated run %v", doer.deleted, want)
	}
	if len(doer.calls) != 2 {
		t.Fatalf("dispatched %d, want pr-updated then checks-failed", len(doer.calls))
	}
}

func TestChecksFailedCancelsTheOpenedRun(t *testing.T) {
	w, doer := newTestWatcher(t)
	rc := RepoConfig{Repo: "o/r", Workflow: "review-pr", On: []string{EventOpened, EventUpdated, EventChecksFailed}}

	green := pr{Number: 13, HeadRefOid: "a"}
	if _, err := w.handle(rc, green); err != nil { // first sight: pr-opened only
		t.Fatal(err)
	}
	red := green
	red.StatusCheckRollup = []check{{Conclusion: "FAILURE"}}
	if _, err := w.handle(rc, red); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/api/executions/exec-1"}; !slices.Equal(doer.deleted, want) {
		t.Fatalf("deleted %v, want the pr-opened run %v", doer.deleted, want)
	}
	if len(doer.calls) != 2 {
		t.Fatalf("dispatched %d, want pr-opened then checks-failed", len(doer.calls))
	}
}

func TestReviewCommentKeepsTheCommitReview(t *testing.T) {
	w, doer := newTestWatcher(t)
	rc := RepoConfig{Repo: "o/r", Workflow: "review-pr", On: []string{EventUpdated, EventReviewComment}}
	me := author{Login: "me"}

	p := pr{Number: 14, HeadRefOid: "a", Author: me}
	if _, err := w.handle(rc, p); err != nil { // first sight: recorded, not dispatched
		t.Fatal(err)
	}
	p.HeadRefOid = "b"
	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}
	p.Comments = []comment{{ID: "c1", Author: author{Login: "reviewer"}}}
	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}
	if len(doer.deleted) != 0 {
		t.Fatalf("the comment run cancelled the live review: %v", doer.deleted)
	}
	if len(doer.calls) != 2 {
		t.Fatalf("dispatched %d, want pr-updated then review-comment", len(doer.calls))
	}

	// A push while the reply runs cancels the commit review of the old head,
	// not the reply, which still answers its comment.
	p.HeadRefOid = "c"
	if _, err := w.handle(rc, p); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/api/executions/exec-1"}; !slices.Equal(doer.deleted, want) {
		t.Fatalf("deleted %v, want the commit review only %v", doer.deleted, want)
	}
	if len(doer.calls) != 3 {
		t.Fatalf("dispatched %d, want a third run for the push", len(doer.calls))
	}
}

func TestReviewCommentSurvivesAMovedHead(t *testing.T) {
	w, doer := newTestWatcher(t)
	rc := RepoConfig{Repo: "o/r", Workflow: "reply-pr", On: []string{EventUpdated, EventReviewComment}}
	me := author{Login: "me"}
	them := author{Login: "reviewer"}

	p := pr{Number: 16, HeadRefOid: "a", Author: me,
		Comments: []comment{{ID: "c0", Author: me}}}
	if _, err := w.handle(rc, p); err != nil { // first sight: recorded, not dispatched
		t.Fatal(err)
	}
	p.Comments = append(p.Comments, comment{ID: "c1", Author: them})
	if _, err := w.handle(rc, p); err != nil { // review-comment, head a
		t.Fatal(err)
	}
	p.HeadRefOid = "b"
	if _, err := w.handle(rc, p); err != nil { // the author pushes while the reply runs
		t.Fatal(err)
	}
	if len(doer.deleted) != 0 {
		t.Fatalf("a push cancelled the reply to an unanswered comment: %v", doer.deleted)
	}
	if len(doer.calls) != 2 {
		t.Fatalf("dispatched %d, want review-comment then pr-updated", len(doer.calls))
	}
}

func TestAcceptedRunWithoutIDKeepsThePreviousOne(t *testing.T) {
	w, doer := newTestWatcher(t)
	rc := RepoConfig{Repo: "o/r", Workflow: "review-pr", On: []string{EventUpdated}}

	if _, err := w.handle(rc, pr{Number: 15, HeadRefOid: "a"}); err != nil { // first sight: recorded
		t.Fatal(err)
	}
	if _, err := w.handle(rc, pr{Number: 15, HeadRefOid: "b"}); err != nil {
		t.Fatal(err)
	}
	doer.emptyID = true
	if _, err := w.handle(rc, pr{Number: 15, HeadRefOid: "c"}); err != nil {
		t.Fatal(err)
	}
	if len(doer.deleted) != 0 {
		t.Fatalf("an id-less run cancelled the live run: %v", doer.deleted)
	}

	// The stored entry still names the run awf will cancel: the next head
	// supersedes it and records its own id.
	doer.emptyID = false
	if _, err := w.handle(rc, pr{Number: 15, HeadRefOid: "d"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/api/executions/exec-1"}; !slices.Equal(doer.deleted, want) {
		t.Fatalf("deleted %v, want the run stored before the id-less one %v", doer.deleted, want)
	}
}

func TestCorruptExecFileRebuilds(t *testing.T) {
	w, doer := newTestWatcher(t)
	rc := RepoConfig{Repo: "o/r", Workflow: "review-pr", On: []string{EventUpdated}}

	if _, err := w.handle(rc, pr{Number: 18, HeadRefOid: "a"}); err != nil { // first sight
		t.Fatal(err)
	}
	if err := os.WriteFile(w.state.execPath(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.handle(rc, pr{Number: 18, HeadRefOid: "b"}); err != nil { // tracks exec-1 anyway
		t.Fatal(err)
	}
	if _, err := w.handle(rc, pr{Number: 18, HeadRefOid: "c"}); err != nil { // cancels it
		t.Fatal(err)
	}
	if want := []string{"/api/executions/exec-1"}; !slices.Equal(doer.deleted, want) {
		t.Fatalf("deleted %v, want the run stored after the rebuild %v", doer.deleted, want)
	}
	if _, err := w.state.loadExec(); err != nil {
		t.Fatalf("the corrupt sidecar was not rewritten: %v", err)
	}
}

func TestNullExecFileDoesNotPanic(t *testing.T) {
	w, doer := newTestWatcher(t)
	rc := RepoConfig{Repo: "o/r", Workflow: "review-pr", On: []string{EventUpdated}}

	if err := os.WriteFile(w.state.execPath(), []byte("null"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.handle(rc, pr{Number: 19, HeadRefOid: "a"}); err != nil { // first sight
		t.Fatal(err)
	}
	if _, err := w.handle(rc, pr{Number: 19, HeadRefOid: "b"}); err != nil { // tracks exec-1
		t.Fatal(err)
	}
	if _, err := w.handle(rc, pr{Number: 19, HeadRefOid: "c"}); err != nil { // cancels it
		t.Fatal(err)
	}
	if want := []string{"/api/executions/exec-1"}; !slices.Equal(doer.deleted, want) {
		t.Fatalf("deleted %v, want the run stored after the null sidecar %v", doer.deleted, want)
	}
}

func TestAnotherWorkflowKeepsItsRun(t *testing.T) {
	w, doer := newTestWatcher(t)
	me := author{Login: "me"}
	review := RepoConfig{Repo: "o/r", Workflow: "review-pr", On: []string{EventUpdated}}
	reply := RepoConfig{Repo: "o/r", Workflow: "reply-pr", On: []string{EventReviewComment}}

	p := pr{Number: 12, HeadRefOid: "a", Author: me}
	if _, err := w.handle(review, p); err != nil { // first sight: recorded
		t.Fatal(err)
	}
	p.HeadRefOid = "b"
	if _, err := w.handle(review, p); err != nil {
		t.Fatal(err)
	}
	p.Comments = []comment{{ID: "c1", Author: author{Login: "reviewer"}}}
	if _, err := w.handle(reply, p); err != nil { // reply-pr, same PR and head
		t.Fatal(err)
	}
	if len(doer.deleted) != 0 {
		t.Fatalf("reply-pr cancelled review-pr: %v", doer.deleted)
	}
	if len(doer.calls) != 2 {
		t.Fatalf("dispatched %d, want one run per workflow", len(doer.calls))
	}
}

func TestIssueDispatchNeverCancels(t *testing.T) {
	w, doer := newTestWatcher(t)
	// A stored execution for the same repository must stay untouched.
	if err := w.state.saveExec(map[string][]execEntry{
		execKey("o/r", 2, "fix-issue", "review"): {{SHA: "a", ID: "exec-old"}},
	}); err != nil {
		t.Fatal(err)
	}
	rc := RepoConfig{Repo: "o/r", Workflow: "fix-issue", On: []string{EventIssueOpened}}
	if _, err := w.handleIssue(rc, issue{Number: 2, Author: author{Login: "me"}}); err != nil {
		t.Fatal(err)
	}
	if len(doer.deleted) != 0 || len(doer.calls) != 1 {
		t.Fatalf("deleted=%v calls=%v, want one POST and no DELETE", doer.deleted, doer.calls)
	}
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

func TestForgetDropsOnlyMatchingKeys(t *testing.T) {
	path := t.TempDir() + "/seen.jsonl"
	st := newState(path)
	keys := []string{
		"o/r#5#seen",
		"o/r#5#review-comment@111",
		"o/r#5#review-comment@222",
	}
	for _, k := range keys {
		if err := st.add(k); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.forget("222")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("forgot %d keys, want 1", n)
	}

	reloaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.has("o/r#5#review-comment@222") {
		t.Fatal("forgotten key is still recorded")
	}
	for _, k := range keys[:2] {
		if !reloaded.has(k) {
			t.Fatalf("%q disappeared along with the forgotten key", k)
		}
	}

	if n, err := st.forget("nothing-matches"); err != nil || n != 0 {
		t.Fatalf("forget with no match = %d, %v; want 0, nil", n, err)
	}
}

func TestReviewBodyAndInlineCommentsAreAnswerable(t *testing.T) {
	me := author{Login: "me"}
	them := author{Login: "them"}
	base := pr{Number: 7, HeadRefOid: "sha", Author: me, Comments: []comment{
		{ID: "IC_1", Author: them, Body: "look", CreatedAt: "2026-11-22T10:00:00Z"},
		{ID: "IC_2", Author: me, Body: "done", CreatedAt: "2026-11-22T11:00:00Z"},
	}}

	// My reply closes the streak, so the newer review body is what is owed.
	withReview := base
	withReview.Reviews = []comment{
		{ID: "PRR_1", Author: them, Body: "one nit", Submitted: "2026-11-22T12:00:00Z"},
	}
	if !withReview.answerable() {
		t.Fatal("review body after my comment is not answerable")
	}
	if got := withReview.ref(EventReviewComment); got != "PRR_1" {
		t.Fatalf("ref = %q, want the review body PRR_1", got)
	}

	// A review body only carries inline comments, so they decide.
	inlineOnly := pr{Number: 7, HeadRefOid: "sha", Author: me, Reviews: []comment{
		{ID: "PRR_2", Author: them, Submitted: "2026-11-22T12:00:00Z"},
	}, Inline: []comment{
		{ID: "PRRC_1", Author: them, Body: "here", CreatedAt: "2026-11-22T12:30:00Z"},
	}}
	if !inlineOnly.answerable() {
		t.Fatal("inline review comment is not answerable")
	}
	if got := inlineOnly.ref(EventReviewComment); got != "PRRC_1" {
		t.Fatalf("ref = %q, want the inline comment PRRC_1", got)
	}

	// My own inline reply ends the streak again.
	answered := inlineOnly
	answered.Inline = append(answered.Inline,
		comment{ID: "PRRC_2", Author: me, Body: "fixed", CreatedAt: "2026-11-22T13:00:00Z"})
	if answered.answerable() {
		t.Fatal("my own inline reply still looks answerable")
	}
}

func TestBotCommentsAreNotAnswerable(t *testing.T) {
	me := author{Login: "me"}
	p := pr{Number: 8, HeadRefOid: "sha", Author: me, Comments: []comment{
		{ID: "c1", Author: me, Body: "done", CreatedAt: "2026-11-22T10:00:00Z"},
		{ID: "c2", Author: author{Login: "codex-connector[bot]"}, Body: "receipt",
			CreatedAt: "2026-11-22T11:00:00Z"},
	}}
	if p.answerable() {
		t.Fatal("a bot receipt raised a review-comment event")
	}

	// A bot receipt after a reviewer must not hide the review behind it.
	q := pr{Number: 9, HeadRefOid: "sha", Author: me, Comments: []comment{
		{ID: "c1", Author: author{Login: "reviewer"}, Body: "one nit",
			CreatedAt: "2026-11-22T10:00:00Z"},
		{ID: "c2", Author: author{Login: "codex-connector[bot]"}, Body: "receipt",
			CreatedAt: "2026-11-22T11:00:00Z"},
	}}
	if !q.answerable() {
		t.Fatal("a bot receipt hid the reviewer comment behind it")
	}
	if got := q.ref(EventReviewComment); got != "c1" {
		t.Fatalf("ref = %q, want the reviewer comment c1", got)
	}
}

func TestIssueLabelFiresOncePerLabel(t *testing.T) {
	w, doer := newTestWatcher(t)
	rc := RepoConfig{Repo: "o/r", Workflow: "fix-issue", On: []string{EventIssueLabeled}, Labels: []string{"dot"}}
	iss := issue{Number: 2, Title: "README lie", Author: author{Login: "me"},
		Labels: []label{{Name: "bug"}, {Name: "dot"}}}

	stop, err := w.handleIssue(rc, iss)
	if err != nil || stop {
		t.Fatalf("issue: stop=%v err=%v", stop, err)
	}
	if len(doer.calls) != 1 || !strings.Contains(doer.calls[0], `"issue":"2"`) {
		t.Fatalf("labeled issue dispatched %v, want one with issue 2", doer.calls)
	}

	// Same label, next pass: nothing.
	if _, err := w.handleIssue(rc, iss); err != nil {
		t.Fatal(err)
	}
	if len(doer.calls) != 1 {
		t.Fatalf("same label dispatched twice: %v", doer.calls[1:])
	}

	// No watched label: nothing, and the author filters still apply.
	other := issue{Number: 3, Author: author{Login: "me"}, Labels: []label{{Name: "bug"}}}
	if _, err := w.handleIssue(rc, other); err != nil {
		t.Fatal(err)
	}
	if len(doer.calls) != 1 {
		t.Fatalf("issue without the label dispatched: %v", doer.calls[1:])
	}

	rc.IgnoreAuthors = []string{"bot"}
	bot := issue{Number: 4, Author: author{Login: "bot"}, Labels: []label{{Name: "dot"}}}
	if _, err := w.handleIssue(rc, bot); err != nil {
		t.Fatal(err)
	}
	if len(doer.calls) != 1 {
		t.Fatalf("ignored author dispatched: %v", doer.calls[1:])
	}
}

func TestIssueLabelNeedsALabel(t *testing.T) {
	cfg := &Config{Repos: []RepoConfig{{
		Repo: "o/r", Workflow: "fix-issue", On: []string{EventIssueLabeled},
	}}}
	if err := cfg.validate(); err == nil {
		t.Fatal("issue-labeled without labels passed validation")
	}
}
