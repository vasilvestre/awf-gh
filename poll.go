package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

const (
	EventOpened        = "pr-opened"
	EventUpdated       = "pr-updated"
	EventChecksFailed  = "checks-failed"
	EventReviewComment = "review-comment"
	EventIssueOpened   = "issue-opened"
	EventIssueLabeled  = "issue-labeled"
)

var allEvents = []string{EventOpened, EventUpdated, EventChecksFailed, EventReviewComment, EventIssueOpened, EventIssueLabeled}

// pr keeps only the fields the dedup key needs. Reading the body or the diff
// is the workflow's job, not the watcher's.
type pr struct {
	Number            int       `json:"number"`
	HeadRefOid        string    `json:"headRefOid"`
	IsDraft           bool      `json:"isDraft"`
	Author            author    `json:"author"`
	Comments          []comment `json:"comments"`
	Reviews           []comment `json:"reviews"`
	Inline            []comment `json:"-"`
	StatusCheckRollup []check   `json:"statusCheckRollup"`
}

type author struct {
	Login string `json:"login"`
}

type comment struct {
	ID        string `json:"id"`
	Author    author `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	Submitted string `json:"submittedAt"`
}

// at is the position of the comment in time. Conversation comments carry
// createdAt, reviews carry submittedAt.
func (c comment) at() time.Time {
	for _, raw := range []string{c.CreatedAt, c.Submitted} {
		if raw == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

type check struct {
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

func (p pr) failing() bool {
	return slices.ContainsFunc(p.StatusCheckRollup, func(c check) bool {
		switch c.Conclusion {
		case "FAILURE", "TIMED_OUT", "CANCELLED":
			return true
		}
		return c.State == "FAILURE" || c.State == "ERROR"
	})
}

// events returns the configured events this PR currently satisfies.
func (p pr) events(wanted []string) []string {
	var out []string
	if slices.Contains(wanted, EventOpened) {
		out = append(out, EventOpened)
	}
	// A red check and a push of the same commit are one review: the run with
	// the logs supersedes the run without, so pr-updated is dropped then.
	if slices.Contains(wanted, EventChecksFailed) && p.failing() {
		out = append(out, EventChecksFailed)
	} else if slices.Contains(wanted, EventUpdated) {
		out = append(out, EventUpdated)
	}
	if slices.Contains(wanted, EventReviewComment) && p.answerable() {
		out = append(out, EventReviewComment)
	}
	return out
}

// newest is the latest thing the reviewer could have written: a conversation
// comment, a review body or an inline review comment. A review with an empty
// body only carries inline comments, which are collected separately.
func (p pr) newest() (comment, bool) {
	var best comment
	var found bool
	for _, set := range [][]comment{p.Comments, p.Reviews, p.Inline} {
		for _, c := range set {
			if c.ID == "" || (c.Body != "" && strings.TrimSpace(c.Body) == "") {
				continue
			}
			// A bot posts receipts and status lines, not remarks to answer. Skipping
			// it here keeps answerable() and ref() reading the same feedback.
			if strings.HasSuffix(c.Author.Login, "[bot]") {
				continue
			}
			// Ties keep the later entry: list order is chronological inside one
			// source, and a review is newer than the comment it answers.
			if !found || !c.at().Before(best.at()) {
				best, found = c, true
			}
		}
	}
	return best, found
}

// answerable reports whether the newest comment comes from someone other than
// the PR author: a reply is owed, or at least possible. The watcher's own
// comment ends the streak, so the event does not loop.
func (p pr) answerable() bool {
	c, ok := p.newest()
	return ok && c.Author.Login != p.Author.Login
}

// ref is the dedup payload: the head commit for the commit events, the newest
// comment for review-comment.
func (p pr) ref(event string) string {
	if event == EventReviewComment {
		c, _ := p.newest()
		return c.ID
	}
	return p.HeadRefOid
}

// eventKey is the dedup identity. pr-updated and checks-failed are keyed on
// the head commit, so a new push re-fires while a bot comment does not.
// review-comment is keyed on the comment id, so every new comment fires once.
func eventKey(repo string, number int, event, ref string) string {
	if event == EventOpened {
		return fmt.Sprintf("%s#%d#%s", repo, number, event)
	}
	return fmt.Sprintf("%s#%d#%s@%s", repo, number, event, ref)
}

// seenKey marks a PR as known, whatever events the config asks for.
func seenKey(repo string, number int) string {
	return fmt.Sprintf("%s#%d#seen", repo, number)
}

// execKey names one execution slot: the workflow, the kind of run (commit
// review or comment reply), and the pull request. The workflow is part of the
// key so two configured workflows on one repository never cancel each other's
// live run.
func execKey(repo string, number int, workflow, class string) string {
	return fmt.Sprintf("%s#%d#%s#%s", repo, number, workflow, class)
}

func listPRs(repo string) ([]pr, error) {
	cmd := exec.Command("gh", "pr", "list",
		"--repo", repo,
		"--state", "open",
		"--limit", "100",
		"--json", "number,headRefOid,isDraft,author,comments,reviews,statusCheckRollup",
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh pr list: %s", ee.Stderr)
		}
		return nil, err
	}
	var prs []pr
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	return prs, nil
}

// inlineComments reads the comments left on diff lines. gh pr list does not
// expose them, and they cost one call per pull request, so the caller asks
// only when a review exists.
func inlineComments(repo string, number int) ([]comment, error) {
	cmd := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/pulls/%d/comments?per_page=100", repo, number),
		"--jq", `[.[] | {id: .node_id, author: {login: .user.login}, body, createdAt: .created_at}]`)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh api: %s", ee.Stderr)
		}
		return nil, err
	}
	var cs []comment
	if err := json.Unmarshal(out, &cs); err != nil {
		return nil, fmt.Errorf("gh api: %w", err)
	}
	return cs, nil
}

// issue keeps what the issue triggers need: the number for the key and the
// inputs, the labels to match, the author for the same filters as pull
// requests.
type issue struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Labels []label `json:"labels"`
	Author author  `json:"author"`
}

type label struct {
	Name string `json:"name"`
}

// labels returns the labels present on the issue and asked for by the config,
// in config order, so the dispatch order is stable.
func (i issue) labels(wanted []string) []string {
	var out []string
	for _, want := range wanted {
		if slices.ContainsFunc(i.Labels, func(l label) bool { return l.Name == want }) {
			out = append(out, want)
		}
	}
	return out
}

// issueKey is the dedup identity: one dispatch per issue and event, plus the
// label for issue-labeled. Adding the label again does not fire twice;
// `--forget repo#issue<N>` replays it.
func issueKey(repo string, number int, event, label string) string {
	if label == "" {
		return fmt.Sprintf("%s#issue%d#%s", repo, number, event)
	}
	return fmt.Sprintf("%s#issue%d#%s@%s", repo, number, event, label)
}

func listIssues(repo string) ([]issue, error) {
	cmd := exec.Command("gh", "issue", "list",
		"--repo", repo,
		"--state", "open",
		"--limit", "100",
		"--json", "number,title,labels,author",
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh issue list: %s", ee.Stderr)
		}
		return nil, err
	}
	var issues []issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("gh issue list: %w", err)
	}
	return issues, nil
}
