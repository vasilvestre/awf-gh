// Command awf-gh turns open pull requests into awf workflow executions.
//
// It polls gh, derives a few events, and POSTs to a running `awf serve`.
// One-shot by design: pair it with a systemd timer or ztick.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "awf-gh:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cfgPath = flag.String("config", defaultConfigPath(), "path to config.yaml")
		dryRun  = flag.Bool("dry-run", false, "print what would be dispatched, record nothing")
		seed    = flag.Bool("seed", false, "record the current PRs as seen without dispatching (first run)")
		forget  = flag.String("forget", "", "drop recorded keys containing this text (a comment id, or repo#number), then exit")
	)
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}

	st, err := loadState(cfg.StatePath)
	if *forget != "" {
		// Forgetting a whole repo#number also drops the "seen" marker, and a PR
		// seen for the first time only fires pr-opened. Forget the comment id.
		switch {
		case errors.Is(err, errNoState):
			return fmt.Errorf("%w: nothing to forget in %s", err, cfg.StatePath)
		case err != nil:
			return err
		}
		n, err := st.forget(*forget)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no key matches %q in %s", *forget, cfg.StatePath)
		}
		fmt.Printf("forgot %d key(s) matching %q, the next run replays them\n", n, *forget)
		return nil
	}
	switch {
	case *seed:
		// Seeding reports the full picture, so it starts from an empty set.
		st = newState(cfg.StatePath)
	case errors.Is(err, errNoState):
		if !*dryRun {
			return fmt.Errorf("%w; run once with --seed to skip the existing backlog", err)
		}
		// A dry run has nothing recorded yet, so it reports the backlog.
		st = newState(cfg.StatePath)
	case err != nil:
		return err
	}

	w := &watcher{
		cfg:    cfg,
		state:  st,
		client: newHTTPClient(),
		dryRun: *dryRun,
		seed:   *seed,
	}
	return w.poll()
}

type watcher struct {
	cfg        *Config
	state      *state
	client     httpDoer
	dryRun     bool
	seed       bool
	dispatched int
}

func (w *watcher) poll() error {
	for _, rc := range w.cfg.Repos {
		prs, err := listPRs(rc.Repo)
		if err != nil {
			// One broken repo must not stop the others.
			fmt.Fprintf(os.Stderr, "awf-gh: %s: %v\n", rc.Repo, err)
			continue
		}
		for _, p := range prs {
			if p.IsDraft || rc.ignores(p.Author.Login) || !rc.allows(p.Author.Login) {
				continue
			}
			// Inline review comments never appear in `gh pr list`, and they cost
			// one call per pull request, so they are fetched only where a review
			// exists and nothing else already needs an answer.
			if slices.Contains(rc.On, EventReviewComment) && len(p.Reviews) > 0 && !p.answerable() {
				cs, err := inlineComments(rc.Repo, p.Number)
				if err != nil {
					fmt.Fprintf(os.Stderr, "awf-gh: %s#%d inline comments: %v\n", rc.Repo, p.Number, err)
				}
				p.Inline = cs
			}
			stop, err := w.handle(rc, p)
			if err != nil {
				return err
			}
			if stop {
				fmt.Fprintf(os.Stderr, "awf-gh: max_per_run=%d reached, the rest waits for the next run\n", w.cfg.MaxPerRun)
				return nil
			}
		}
		if !slices.Contains(rc.On, EventIssueOpened) && !slices.Contains(rc.On, EventIssueLabeled) {
			continue
		}
		issues, err := listIssues(rc.Repo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "awf-gh: %s: %v\n", rc.Repo, err)
			continue
		}
		for _, iss := range issues {
			stop, err := w.handleIssue(rc, iss)
			if err != nil {
				return err
			}
			if stop {
				fmt.Fprintf(os.Stderr, "awf-gh: max_per_run=%d reached, the rest waits for the next run\n", w.cfg.MaxPerRun)
				return nil
			}
		}
	}
	return nil
}

// handleIssue dispatches one issue and reports whether the per-run cap was
// hit. issue-labeled fires once per watched label, issue-opened once.
func (w *watcher) handleIssue(rc RepoConfig, iss issue) (bool, error) {
	if rc.ignores(iss.Author.Login) || !rc.allows(iss.Author.Login) {
		return false, nil
	}
	type fire struct{ event, label string }
	var fires []fire
	if slices.Contains(rc.On, EventIssueOpened) {
		fires = append(fires, fire{EventIssueOpened, ""})
	}
	if slices.Contains(rc.On, EventIssueLabeled) {
		for _, lb := range iss.labels(rc.Labels) {
			fires = append(fires, fire{EventIssueLabeled, lb})
		}
	}
	for _, f := range fires {
		key := issueKey(rc.Repo, iss.Number, f.event, f.label)
		if w.state.has(key) {
			continue
		}
		at := ""
		if f.label != "" {
			at = " @" + f.label
		}
		if w.dryRun {
			fmt.Printf("would dispatch %s %s#%d%s\n", f.event, rc.Repo, iss.Number, at)
			w.state.remember(key)
			continue
		}
		if !w.seed {
			if w.dispatched >= w.cfg.MaxPerRun {
				return true, nil
			}
			if err := dispatchIssue(w.client, w.cfg, rc, iss, f.event, f.label); err != nil {
				// Not recorded, so the next run retries it.
				fmt.Fprintf(os.Stderr, "awf-gh: %s#%d %s: %v\n", rc.Repo, iss.Number, f.event, err)
				continue
			}
			fmt.Printf("dispatched %s %s#%d%s -> %s/%s\n", f.event, rc.Repo, iss.Number, at, rc.Scope, rc.Workflow)
			w.dispatched++
		}
		if err := w.record(key); err != nil {
			return false, err
		}
	}
	return false, nil
}

// handle processes one PR and reports whether the per-run cap was hit.
func (w *watcher) handle(rc RepoConfig, p pr) (bool, error) {
	firstSight := !w.state.has(seenKey(rc.Repo, p.Number))

	for _, ev := range p.events(rc.On) {
		key := eventKey(rc.Repo, p.Number, ev, p.ref(ev))
		if w.state.has(key) {
			continue
		}
		// A PR met for the first time only gets pr-opened. Its other events
		// are recorded silently, so they fire on the next push, not now.
		if firstSight && ev != EventOpened {
			if err := w.record(key); err != nil {
				return false, err
			}
			continue
		}
		if w.dryRun {
			fmt.Printf("would dispatch %s %s#%d @%s\n", ev, rc.Repo, p.Number, short(p.ref(ev)))
			w.state.remember(key)
			continue
		}
		if !w.seed {
			if w.dispatched >= w.cfg.MaxPerRun {
				return true, nil
			}
			id, err := dispatch(w.client, w.cfg, rc, p, ev)
			if err != nil {
				// Not recorded, so the next run retries it.
				fmt.Fprintf(os.Stderr, "awf-gh: %s#%d %s: %v\n", rc.Repo, p.Number, ev, err)
				continue
			}
			w.replaceExec(rc, p, ev, id)
			fmt.Printf("dispatched %s %s#%d @%s -> %s/%s\n", ev, rc.Repo, p.Number, short(p.ref(ev)), rc.Scope, rc.Workflow)
			w.dispatched++
		}
		if err := w.record(key); err != nil {
			return false, err
		}
	}
	if err := w.record(seenKey(rc.Repo, p.Number)); err != nil {
		return false, err
	}
	return false, nil
}

// replaceExec cancels the run that the accepted one supersedes, then stores
// it. The cancel comes after the POST, so a failed dispatch leaves the
// previous run alive. A failed cancel is logged, never fatal.
func (w *watcher) replaceExec(rc RepoConfig, p pr, event, id string) {
	// An id-less run cannot be stored or cancelled later, so it replaces
	// nothing: keep the previous entry rather than killing it for no record.
	if id == "" {
		return
	}
	execs, err := w.state.loadExec()
	if err != nil {
		// A corrupt sidecar must not stop every later dispatch from tracking
		// its run: start from an empty map, which rewrites the file on save.
		fmt.Fprintf(os.Stderr, "awf-gh: %s#%d exec map: %v\n", rc.Repo, p.Number, err)
		execs = map[string]execEntry{}
	}
	key := execKey(rc.Repo, p.Number, rc.Workflow)
	if prev, ok := execs[key]; ok && supersedes(prev, event) {
		if err := cancel(w.client, w.cfg, prev.ID); err != nil {
			// Keep the previous entry so a later dispatch retries the cancel;
			// overwriting it forgets a run that is still live.
			fmt.Fprintf(os.Stderr, "awf-gh: %s#%d cancel %s: %v\n", rc.Repo, p.Number, prev.ID, err)
			return
		}
	}
	execs[key] = execEntry{SHA: p.HeadRefOid, Event: event, ID: id}
	if err := w.state.saveExec(execs); err != nil {
		fmt.Fprintf(os.Stderr, "awf-gh: %s#%d exec map: %v\n", rc.Repo, p.Number, err)
	}
}

// supersedes reports whether the accepted run replaces prev. Two commit
// reviews, or two replies to different comments, replace each other. A commit
// review and a reply stay alive side by side and both post, even when a push
// moved the head: the reply still answers its comment.
func supersedes(prev execEntry, event string) bool {
	if prev.ID == "" {
		return false
	}
	return (prev.Event == EventReviewComment) == (event == EventReviewComment)
}

// record persists a key, or only remembers it during a dry run.
func (w *watcher) record(key string) error {
	if w.dryRun {
		w.state.remember(key)
		return nil
	}
	return w.state.add(key)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
