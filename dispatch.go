package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// dispatch fires one workflow execution and returns the id awf accepted. The id
// is empty when awf answers without one. The run is async on the awf side: the
// workflow posts its own review, so the watcher never waits for a result.
func dispatch(client httpDoer, cfg *Config, rc RepoConfig, p pr, event string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"inputs": map[string]any{
			"repo":  rc.Repo,
			"pr":    strconv.Itoa(p.Number),
			"sha":   p.HeadRefOid,
			"event": event,
		},
	})
	if err != nil {
		return "", err
	}

	return post(client, cfg, rc, body)
}

func post(client httpDoer, cfg *Config, rc RepoConfig, body []byte) (string, error) {
	url := fmt.Sprintf("%s/api/workflows/%s/%s/run", cfg.AwfURL, rc.Scope, rc.Workflow)
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("POST %s: %s: %s", url, resp.Status, bytes.TrimSpace(msg))
	}

	// awf answers 202 with {"body":{"execution_id":"..."}}. A 2xx without one
	// is still an accepted run, so a decode failure is not a dispatch failure.
	var accepted struct {
		Body struct {
			ExecutionID string `json:"execution_id"`
		} `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&accepted); err != nil {
		return "", nil
	}
	return accepted.Body.ExecutionID, nil
}

// cancel stops a live execution. awf answers 204, including for an id it no
// longer knows, so a cancel is idempotent. A 404 therefore names a route the
// server does not expose: log it, because every cancel is then a no-op.
func cancel(client httpDoer, cfg *Config, id string) error {
	target := fmt.Sprintf("%s/api/executions/%s", cfg.AwfURL, url.PathEscape(id))
	req, err := http.NewRequest(http.MethodDelete, target, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		fmt.Fprintf(os.Stderr, "awf-gh: DELETE %s: 404, check that awf serve exposes this route\n", target)
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("DELETE %s: %s: %s", target, resp.Status, bytes.TrimSpace(msg))
}

// dispatchIssue fires the workflow for one open issue. The issue number is
// an input, not a pull request number: the workflow decides what to do with it.
func dispatchIssue(client httpDoer, cfg *Config, rc RepoConfig, iss issue, event, label string) error {
	inputs := map[string]any{
		"repo":  rc.Repo,
		"issue": strconv.Itoa(iss.Number),
		"event": event,
	}
	if label != "" {
		inputs["label"] = label
	}
	body, err := json.Marshal(map[string]any{"inputs": inputs})
	if err != nil {
		return err
	}
	_, err = post(client, cfg, rc, body)
	return err
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// httpDoer narrows *http.Client so tests can record dispatches and cancels.
type httpDoer interface {
	Post(url, contentType string, body io.Reader) (*http.Response, error)
	Do(req *http.Request) (*http.Response, error)
}
