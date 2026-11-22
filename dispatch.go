package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// dispatch fires one workflow execution and returns once awf accepts it.
// The run is async on the awf side: the workflow posts its own review, so the
// watcher never waits for a result.
func dispatch(client httpDoer, cfg *Config, rc RepoConfig, p pr, event string) error {
	body, err := json.Marshal(map[string]any{
		"inputs": map[string]any{
			"repo":  rc.Repo,
			"pr":    strconv.Itoa(p.Number),
			"sha":   p.HeadRefOid,
			"event": event,
		},
	})
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/api/workflows/%s/%s/run", cfg.AwfURL, rc.Scope, rc.Workflow)
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("POST %s: %s: %s", url, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// httpDoer narrows *http.Client so tests can record dispatches.
type httpDoer interface {
	Post(url, contentType string, body io.Reader) (*http.Response, error)
}
