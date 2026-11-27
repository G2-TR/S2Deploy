package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

type hooks struct {
	s                                    *service
	githubSecret, gitlabToken, larkToken string
}

func (h hooks) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /api/skytek/GitlabWebhook", h.push("work"))
	mux.HandleFunc("POST /api/skytek/GitlabWebhookG2", h.push("g2"))
	mux.HandleFunc("POST /api/skytek/WorkflowWebhook", h.workflow)
	mux.HandleFunc("POST /api/skytek/LarkWebhook", h.challenge)
	// Legacy CreateDeploy was a no-op, retain the endpoint for compatibility.
	mux.HandleFunc("GET /api/skytek/CreateDeploy", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return mux
}

func (h hooks) push(group string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !hmac.Equal([]byte(h.gitlabToken), []byte(r.Header.Get("X-Gitlab-Token"))) {
			w.WriteHeader(401)
			return
		}
		var p struct {
			Ref     string
			User    string `json:"user_name"`
			Commits []struct{ Message string }
		}
		if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&p); e != nil || p.Ref == "" {
			w.WriteHeader(400)
			return
		}
		var msg string
		if len(p.Commits) > 0 {
			msg = p.Commits[0].Message
		}
		card, e := h.s.cards.create(r.Context(), notifyTemplate[group], pushVars(p.Ref, msg, p.User, group))
		if e == nil {
			uuid := r.Header.Get("X-Gitlab-Event-UUID")
			if uuid != "" {
				uuid = shortID(group + ":" + uuid)
			}
			e = h.s.cards.send(r.Context(), group, card, uuid)
		}
		if e != nil {
			log.Printf("GitLab notification: %v", e)
			w.WriteHeader(502)
			return
		}
		w.WriteHeader(204)
	}
}

func (h hooks) challenge(w http.ResponseWriter, r *http.Request) {
	var p struct{ Token, Challenge string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		w.WriteHeader(400)
		return
	}
	if h.larkToken == "" || !hmac.Equal([]byte(p.Token), []byte(h.larkToken)) {
		w.WriteHeader(401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"challenge": p.Challenge})
}

func (h hooks) workflow(w http.ResponseWriter, r *http.Request) {
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if e != nil {
		w.WriteHeader(400)
		return
	}
	mac := hmac.New(sha256.New, []byte(h.githubSecret))
	mac.Write(b)
	if !hmac.Equal([]byte("sha256="+hex.EncodeToString(mac.Sum(nil))), []byte(r.Header.Get("X-Hub-Signature-256"))) {
		w.WriteHeader(401)
		return
	}
	if r.Header.Get("X-GitHub-Event") != "workflow_run" {
		w.WriteHeader(204)
		return
	}
	var p struct {
		Repository struct {
			Name string `json:"full_name"`
		}
		Run run `json:"workflow_run"`
	}
	if e = json.Unmarshal(b, &p); e != nil || p.Run.ID <= 0 {
		w.WriteHeader(400)
		return
	}
	key, ok := workflowTarget(p.Run.Path)
	if !ok || !strings.EqualFold(p.Repository.Name, h.s.repo) {
		w.WriteHeader(204)
		return
	}
	id, tagged := strings.CutPrefix(p.Run.Title, "deploy-")
	if tagged && p.Run.Attempt <= 1 {
		e = h.s.observe(id, p.Run)
	} else if p.Run.Status == "completed" && p.Run.Conclusion != "" {
		e = h.s.manualResult(key, p.Run)
	}
	if e != nil && !errors.Is(e, errUnknown) {
		log.Print(e)
		w.WriteHeader(500)
		return
	}
	h.s.signal()
	w.WriteHeader(204)
}

func (s *store) manualResult(key string, r run) error {
	id := fmt.Sprintf("run-%d-%d", r.ID, r.Attempt)
	return s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(records).Get([]byte(id)) != nil {
			return nil
		}
		return save(tx, deployment{ID: id, Target: key, RunID: r.ID, Created: time.Now().UTC(), Phase: "tracking", Result: r.Conclusion, Started: r.Started, Ended: r.Ended})
	})
}
