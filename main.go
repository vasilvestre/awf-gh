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
	case *seed || *dryRun:
		// Both report the full picture, so they start from an empty set.
		st = newState(cfg.StatePath)
	case errors.Is(err, errNoState):
		return fmt.Errorf("%w; run once with --seed to skip the existing backlog", err)
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
			stop, err := w.handle(rc, p)
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
			if err := dispatch(w.client, w.cfg, rc, p, ev); err != nil {
				// Not recorded, so the next run retries it.
				fmt.Fprintf(os.Stderr, "awf-gh: %s#%d %s: %v\n", rc.Repo, p.Number, ev, err)
				continue
			}
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
