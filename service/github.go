package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

type gitHub struct {
	base, token string
	http        *http.Client
}
type run struct {
	ID                       int64
	Title                    string `json:"display_title"`
	Path, Status, Conclusion string
	Started                  time.Time `json:"run_started_at"`
	Ended                    time.Time `json:"updated_at"`
	Attempt                  int       `json:"run_attempt"`
}
type apiError int

func (e apiError) Error() string { return fmt.Sprintf("GitHub HTTP %d", e) }

func (g gitHub) api(ctx context.Context, method, route string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		b, e := json.Marshal(payload)
		if e != nil {
			return e
		}
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, g.base+route, body)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := g.http.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp.StatusCode)
	}
	if resp.StatusCode == 204 {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (g gitHub) dispatch(ctx context.Context, d deployment) (int64, error) {
	var r struct {
		ID int64 `json:"workflow_run_id"`
	}
	e := g.api(ctx, "POST", "/actions/workflows/"+targets[d.Target].File+"/dispatches", map[string]any{"ref": "main", "inputs": map[string]string{"deployment_id": d.ID}}, &r)
	return r.ID, e
}

func (g gitHub) firstAttempt(ctx context.Context, r run) (run, error) {
	if r.Attempt <= 1 {
		return r, nil
	}
	var first run
	err := g.api(ctx, "GET", fmt.Sprintf("/actions/runs/%d/attempts/1", r.ID), nil, &first)
	return first, err
}

func (g gitHub) lookup(ctx context.Context, d deployment) (run, error) {
	if d.RunID != 0 {
		var r run
		e := g.api(ctx, "GET", fmt.Sprintf("/actions/runs/%d", d.RunID), nil, &r)
		if e != nil {
			return run{}, e
		}
		return g.firstAttempt(ctx, r)
	}
	query := url.Values{"event": {"workflow_dispatch"}, "branch": {"main"}, "created": {">=" + d.Created.Add(-time.Minute).UTC().Format("2006-01-02T15:04:05Z")}, "per_page": {"100"}}
	for page := 1; ; page++ {
		var r struct {
			Runs []run `json:"workflow_runs"`
		}
		route := fmt.Sprintf("/actions/workflows/%s/runs?%s&page=%d", targets[d.Target].File, query.Encode(), page)
		if e := g.api(ctx, "GET", route, nil, &r); e != nil {
			return run{}, e
		}
		for _, v := range r.Runs {
			if v.Title == "deploy-"+d.ID {
				return g.firstAttempt(ctx, v)
			}
		}
		if len(r.Runs) < 100 {
			return run{}, nil
		}
	}
}

func (s *store) observe(id string, r run) error {
	return s.change(id, func(d *deployment) error {
		if r.Title != "deploy-"+d.ID || r.ID <= 0 || path.Base(r.Path) != targets[d.Target].File || (d.RunID != 0 && d.RunID != r.ID) {
			return errors.New("workflow or run mismatch")
		}
		if d.Result != "" {
			return nil
		}
		d.RunID = r.ID
		d.Phase = "tracking"
		d.Progress = r.Status
		if r.Status == "completed" && r.Conclusion != "" {
			d.Result = r.Conclusion
			d.Started, d.Ended = r.Started, r.Ended
		}
		return nil
	})
}

func (g gitHub) steps(ctx context.Context, id int64) (string, error) {
	var list struct {
		Jobs []struct {
			Name, Status string
			Steps        []struct{ Name, Status string }
		}
	}
	if e := g.api(ctx, "GET", fmt.Sprintf("/actions/runs/%d/jobs", id), nil, &list); e != nil {
		return "", e
	}
	var lines []string
	for _, job := range list.Jobs {
		for _, step := range job.Steps {
			if step.Status == "in_progress" {
				lines = append(lines, job.Name+" · "+step.Name)
			}
		}
	}
	return strings.Join(lines, "\n"), nil
}
