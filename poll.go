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
)

var allEvents = []string{EventOpened, EventUpdated, EventChecksFailed, EventReviewComment}

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
	if slices.Contains(wanted, EventUpdated) {
		out = append(out, EventUpdated)
	}
	if slices.Contains(wanted, EventChecksFailed) && p.failing() {
		out = append(out, EventChecksFailed)
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
	if !ok || c.Author.Login == p.Author.Login {
		return false
	}
	// A bot posts receipts and status lines, not remarks to answer.
	return !strings.HasSuffix(c.Author.Login, "[bot]")
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
