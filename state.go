package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errNoState = errors.New("state file does not exist")

// state is an append-only set of dedup keys. One line per dispatched event,
// which doubles as an audit trail.
//
// shortcut: keys are never pruned, the file grows by a line per PR event.
// Rotate it by hand when it reaches a few thousand lines.
type state struct {
	path string
	seen map[string]struct{}
}

func loadState(path string) (*state, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", path, errNoState)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st := &state{path: path, seen: map[string]struct{}{}}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			st.seen[line] = struct{}{}
		}
	}
	return st, sc.Err()
}

func newState(path string) *state {
	return &state{path: path, seen: map[string]struct{}{}}
}

func (s *state) has(key string) bool {
	_, ok := s.seen[key]
	return ok
}

// remember marks a key in memory only. Dry runs use it to stay meaningful.
func (s *state) remember(key string) {
	s.seen[key] = struct{}{}
}

func (s *state) add(key string) error {
	if s.has(key) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, key); err != nil {
		return err
	}
	s.seen[key] = struct{}{}
	return nil
}
