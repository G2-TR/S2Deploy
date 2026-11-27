package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

type fakeCards struct {
	sends                      map[string]int
	seq                        []int
	progressText, workflowLink string
	failFinal                  bool
}

func (f *fakeCards) create(_ context.Context, tmpl string, vars map[string]any) (string, error) {
	return "card-" + tmpl, nil
}
func (f *fakeCards) send(_ context.Context, group, card, id string) error {
	if f.sends == nil {
		f.sends = make(map[string]int)
	}
	f.sends[group+"/"+id]++
	return nil
}
func (f *fakeCards) updateProgress(_ context.Context, _ string, seq int, body, link string) error {
	f.progressText, f.workflowLink = body, link
	f.seq = append(f.seq, seq)
	if f.failFinal && strings.Contains(body, "部署成功") {
		return errors.New("card patch unavailable")
	}
	return nil
}
func requireOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func testStore(t *testing.T, filename string) *store {
	t.Helper()
	s, err := openStore(filename)
	requireOK(t, err)
	return s
}
func record(t *testing.T, s *store, id string) deployment {
	t.Helper()
	d, err := s.get(id)
	requireOK(t, err)
	return d
}

func TestSimultaneousClicksAndReplay(t *testing.T) {
	s := testStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer s.db.Close()
	results := make(chan error, 24)
	var wg sync.WaitGroup
	for i := range cap(results) {
		wg.Add(1)
		go func() { defer wg.Done(); event := fmt.Sprint(i); results <- s.request(event, shortID(event), "main") }()
	}
	wg.Wait()
	close(results)
	accepted := 0
	for e := range results {
		if e == nil {
			accepted++
		} else if !errors.Is(e, busy) {
			t.Fatal(e)
		}
	}
	items, err := s.list()
	requireOK(t, err)
	if accepted != 1 || len(items) != 1 {
		t.Fatalf("%d accepted, %d records", accepted, len(items))
	}
	requireOK(t, s.request("other", "other", "demo"))
	requireOK(t, s.change(items[0].ID, func(d *deployment) error { d.Result = "success"; return nil }))
	for i := range cap(results) {
		event := fmt.Sprint(i)
		e := s.request(event, shortID(event), "main")
		if (e == nil) != (shortID(event) == items[0].ID) {
			t.Fatalf("replay %s: %v", event, e)
		}
	}
	requireOK(t, s.request("intentional", "next", "main"))
}

func click(t *testing.T, s *service) string {
	t.Helper()
	var ev callback.CardActionTriggerEvent
	requireOK(t, json.Unmarshal([]byte(`{"header":{"event_id":"click-81"},"event":{"action":{"value":{"server":"main","branch":"refs/heads/master"}},"context":{"open_chat_id":"work-chat"}}}`), &ev))
	res, err := s.button(map[string]string{"work": "work-chat"})(t.Context(), &ev)
	requireOK(t, err)
	if res.Toast.Type != "success" {
		t.Fatal(res.Toast)
	}
	return shortID("click-81")
}

func TestDispatchWebhookAndCardRecovery(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "state.db")
	id := shortID("click-81")
	var posts atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/actions/workflows/build.yml/dispatches":
			posts.Add(1)
			var req struct {
				Ref    string
				Inputs map[string]string
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Ref != "main" || len(req.Inputs) != 1 || req.Inputs["deployment_id"] != id {
				t.Errorf("dispatch body %+v error %v", req, err)
			}
			fmt.Fprint(w, `{"workflow_run_id":81}`)
		case "/actions/runs/81":
			json.NewEncoder(w).Encode(run{ID: 81, Title: "deploy-" + id, Path: ".github/workflows/build.yml", Status: "queued", Attempt: 1})
		default:
			t.Errorf("unexpected GitHub route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer gh.Close()
	cards := &fakeCards{failFinal: true}
	s := &service{store: testStore(t, filename), repo: "org/deploy", cards: cards, github: gitHub{gh.URL, "test", gh.Client()}, wake: make(chan struct{}, 1)}
	defer func() { s.db.Close() }()
	click(t, s)
	requireOK(t, s.process(t.Context(), record(t, s.store, id)))
	if record(t, s.store, id).RunID != 81 {
		t.Fatal("missing dispatch run ID")
	}
	hook := (hooks{s: s, githubSecret: "hook-secret"}).routes()
	body := jsonString(map[string]any{"repository": map[string]string{"full_name": s.repo}, "workflow_run": run{ID: 81, Title: "deploy-" + id, Path: ".github/workflows/build.yml", Status: "completed", Conclusion: "success", Attempt: 1}})
	notify := func(secret string) int {
		req := httptest.NewRequest("POST", "/api/skytek/WorkflowWebhook", strings.NewReader(body))
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("X-GitHub-Event", "workflow_run")
		w := httptest.NewRecorder()
		hook.ServeHTTP(w, req)
		return w.Code
	}
	if notify("wrong") != 401 || notify("hook-secret") != 204 || notify("hook-secret") != 204 {
		t.Fatal("invalid webhook handling")
	}
	if err := s.process(t.Context(), record(t, s.store, id)); err == nil {
		t.Fatal("expected card failure")
	}
	before := record(t, s.store, id)
	if before.Result != "success" || before.Phase == "done" || !before.ResultSent {
		t.Fatalf("lost final result %+v", before)
	}
	requireOK(t, s.db.Close())
	s.store = testStore(t, filename)
	cards.failFinal = false
	requireOK(t, s.process(t.Context(), record(t, s.store, id)))
	final := record(t, s.store, id)
	pending, err := s.list()
	requireOK(t, err)
	if len(pending) != 0 || final.Phase != "done" || final.Sequence <= before.Sequence || final.CardID != before.CardID || len(cards.sends) != 2 || posts.Load() != 1 {
		t.Fatalf("unexpected recovery %+v; sends %v", final, cards.sends)
	}
	for _, n := range cards.sends {
		if n != 1 {
			t.Fatal("duplicate notification")
		}
	}
}

func TestProgressLinkTracksRunWithoutChangingText(t *testing.T) {
	cards := &fakeCards{}
	s := &service{store: testStore(t, filepath.Join(t.TempDir(), "state.db")), cards: cards, repo: "org/deploy"}
	defer s.db.Close()
	const id = "pending-link"
	requireOK(t, s.request("event", id, "main"))
	requireOK(t, s.deliver(t.Context(), id, targets["main"]))
	text := cards.progressText
	if text == "" || strings.Contains(text, "查看工作流") || cards.workflowLink != "[查看工作流](https://github.com/org/deploy/actions/workflows/build.yml)" {
		t.Fatalf("unexpected pending progress: %q %q", text, cards.workflowLink)
	}
	requireOK(t, s.change(id, func(d *deployment) error { d.RunID = 81; return nil }))
	requireOK(t, s.deliver(t.Context(), id, targets["main"]))
	if cards.progressText != text || cards.workflowLink != "[查看工作流](https://github.com/org/deploy/actions/runs/81)" || len(cards.seq) != 2 {
		t.Fatalf("unexpected run progress: %q %q %v", cards.progressText, cards.workflowLink, cards.seq)
	}
}

func TestUncertainDispatchRecoversExactRun(t *testing.T) {
	var posts atomic.Int32
	s := &service{store: testStore(t, filepath.Join(t.TempDir(), "state.db")), cards: &fakeCards{}, repo: "org/repo"}
	defer s.db.Close()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			w.WriteHeader(502)
			return
		}
		items := []run{{ID: 802, Title: "manual", Path: "build.yml", Status: "completed", Conclusion: "failure"}, {ID: 801, Title: "deploy-id81", Path: "build.yml", Status: "completed", Conclusion: "success"}}
		json.NewEncoder(w).Encode(map[string]any{"workflow_runs": items})
	}))
	defer gh.Close()
	s.github = gitHub{gh.URL, "test", gh.Client()}
	requireOK(t, s.request("ev", "id81", "main"))
	if s.process(t.Context(), record(t, s.store, "id81")) == nil {
		t.Fatal("expected uncertain dispatch")
	}
	requireOK(t, s.process(t.Context(), record(t, s.store, "id81")))
	d := record(t, s.store, "id81")
	if d.RunID != 801 || d.Result != "success" || d.Phase != "done" || posts.Load() != 1 {
		t.Fatalf("incorrect association %+v, posts=%d", d, posts.Load())
	}
}

func TestManualG2ResultReplays(t *testing.T) {
	cards := &fakeCards{}
	s := &service{store: testStore(t, filepath.Join(t.TempDir(), "state.db")), cards: cards}
	defer s.db.Close()
	r := run{ID: 90, Attempt: 2, Conclusion: "cancelled"}
	for range 2 {
		requireOK(t, s.manualResult("g2-work", r))
		pending, err := s.list()
		requireOK(t, err)
		for _, d := range pending {
			requireOK(t, s.process(t.Context(), d))
		}
	}
	if len(cards.sends) != 2 || len(cards.seq) != 0 {
		t.Fatalf("expected one result per group, got %+v", cards)
	}
	for _, n := range cards.sends {
		if n != 1 {
			t.Fatal("duplicate result")
		}
	}
}

type pushRecorder struct {
	fakeCards
	template string
	vars     map[string]any
}

func (f *pushRecorder) create(_ context.Context, template string, vars map[string]any) (string, error) {
	f.template, f.vars = template, vars
	return "push-card", nil
}

func TestGitLabPushCards(t *testing.T) {
	cards := &pushRecorder{}
	mux := (hooks{s: &service{cards: cards}, gitlabToken: "test-token"}).routes()
	for _, tc := range []struct {
		route, group, ref string
		disabled          bool
	}{
		{"GitlabWebhook", "work", "refs/heads/master", false},
		{"GitlabWebhookG2", "g2", "refs/heads/prod-merge", false},
		{"GitlabWebhook", "work", "refs/heads/feature", true},
	} {
		body := jsonString(map[string]any{"ref": tc.ref, "user_name": "user", "commits": []map[string]string{{"message": "first"}, {"message": "second"}}})
		req := httptest.NewRequest("POST", "/api/skytek/"+tc.route, strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 401 {
			t.Fatalf("unsigned push %d", w.Code)
		}
		req.Header.Set("X-Gitlab-Token", "test-token")
		req.Header.Set("X-Gitlab-Event-UUID", tc.ref)
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 204 || cards.template != notifyTemplate[tc.group] || cards.vars["commit_message"] != "first" || cards.vars["deploy_disable"] != tc.disabled || cards.sends[tc.group+"/"+shortID(tc.group+":"+tc.ref)] != 1 {
			t.Fatalf("%s: %d %+v", tc.route, w.Code, cards)
		}
	}
}

func TestPollingUsesOriginalAttempt(t *testing.T) {
	const id = "test"
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		run := run{ID: 42, Title: "deploy-" + id, Path: "build.yml", Status: "completed", Conclusion: "success", Attempt: 2}
		switch r.URL.Path {
		case "/actions/runs/42":
		case "/actions/runs/42/attempts/1":
			run.Conclusion = "failure"
			run.Attempt = 1
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(run)
	}))
	defer gh.Close()
	g := gitHub{gh.URL, "test", gh.Client()}
	r, err := g.lookup(t.Context(), deployment{ID: id, RunID: 42, Target: "main"})
	requireOK(t, err)
	if r.Attempt != 1 || r.Conclusion != "failure" {
		t.Fatalf("wrong run attempt %+v", r)
	}
}
