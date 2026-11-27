package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

type cards interface {
	create(context.Context, string, map[string]any) (string, error)
	send(context.Context, string, string, string) error
	patch(context.Context, string, int, string) error
}
type service struct {
	*store
	github gitHub
	cards  cards
	repo   string
	wake   chan struct{}
}

func (s *service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *service) work(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		items, err := s.list()
		if err != nil {
			log.Print(err)
		}
		for _, d := range items {
			if ctx.Err() != nil {
				return
			}
			if e := s.process(ctx, d); e != nil {
				log.Printf("deployment=%s target=%s: %v", d.ID, d.Target, e)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

func (s *service) process(ctx context.Context, d deployment) error {
	t := targets[d.Target]
	if d.Phase == "queued" {
		// Persist before POST. A lost response must never cause a second dispatch.
		if e := s.change(d.ID, func(v *deployment) error { v.Phase = "dispatched"; return nil }); e != nil {
			return e
		}
		id, e := s.github.dispatch(ctx, d)
		if e != nil {
			var status apiError
			if errors.As(e, &status) && status >= 400 && status < 500 && status != 408 {
				return s.change(d.ID, func(v *deployment) error {
					if v.RunID == 0 {
						v.Result = e.Error()
					}
					return nil
				})
			}
			return errors.Join(e, s.deliver(ctx, d.ID, t))
		}
		if id != 0 {
			if e = s.change(d.ID, func(v *deployment) error {
				if v.RunID != 0 && v.RunID != id {
					return errors.New("dispatch response mismatch")
				}
				v.RunID = id
				return nil
			}); e != nil {
				return e
			}
		}
		d.RunID = id
	}
	var pollErr error
	if d.Result == "" {
		r, e := s.github.lookup(ctx, d)
		pollErr = e
		if e == nil && r.ID != 0 {
			if e = s.observe(d.ID, r); e != nil {
				return e
			}
			if r.Status == "in_progress" {
				detail, e := s.github.steps(ctx, r.ID)
				if e == nil && detail != "" {
					if e = s.change(d.ID, func(v *deployment) error {
						if v.Result == "" {
							v.Progress = detail
						}
						return nil
					}); e != nil {
						return e
					}
				}
			}
		}
	}
	if e := s.deliver(ctx, d.ID, t); e != nil {
		return e
	}
	return pollErr
}

func (s *service) deliver(ctx context.Context, id string, t target) error {
	d, err := s.get(id)
	if err != nil || d.Phase == "done" {
		return err
	}
	manual := strings.HasPrefix(id, "run-")
	var progressErr, resultErr error
	if !manual {
		progressErr = s.progressCard(ctx, d, t)
	}
	if d.Result != "" {
		resultErr = s.resultCard(ctx, d, t)
		if progressErr == nil && resultErr == nil {
			return s.change(id, func(v *deployment) error { v.Phase = "done"; return nil })
		}
	}
	return errors.Join(progressErr, resultErr)
}

func (s *service) progressCard(ctx context.Context, d deployment, t target) error {
	id := d.ID
	link := "https://github.com/" + s.repo + "/actions/workflows/" + t.File
	if d.RunID != 0 {
		link = fmt.Sprintf("https://github.com/%s/actions/runs/%d", s.repo, d.RunID)
	}
	message := t.Name + " · 等待 GitHub 确认任务"
	if d.Progress != "" {
		message = t.Name + " · " + d.Progress
	}
	if d.Result != "" {
		message = fmt.Sprintf("**%s** · %s", t.Name, resultText(d.Result))
	}
	message += "\n[查看工作流](" + link + ")"
	if d.CardID == "" {
		card, e := s.cards.create(ctx, "AAqXqlNu4g67k", map[string]any{"server": t.Name, "workflow_url": map[string]string{"url": link}, "deploy_process_text": message})
		if e != nil {
			return e
		}
		if e = s.change(id, func(v *deployment) error { v.CardID = card; return nil }); e != nil {
			return e
		}
		d.CardID = card
	}
	if !d.Sent {
		if e := s.cards.send(ctx, t.Group, d.CardID, id); e != nil {
			return e
		}
		if e := s.change(id, func(v *deployment) error { v.Sent = true; return nil }); e != nil {
			return e
		}
	}
	if d.Delivered == message {
		return nil
	}
	// Reserve the sequence before calling Feishu so a restarted worker always advances it.
	if err := s.change(id, func(v *deployment) error { v.Sequence++; d.Sequence = v.Sequence; return nil }); err != nil {
		return err
	}
	if err := s.cards.patch(ctx, d.CardID, d.Sequence, message); err != nil {
		return err
	}
	return s.change(id, func(v *deployment) error { v.Delivered = message; return nil })
}

func (s *service) resultCard(ctx context.Context, d deployment, t target) error {
	if d.ResultCardID == "" {
		template := "AAqXmOd4WPgx1"
		end := d.Ended
		if end.IsZero() {
			end = time.Now()
		}
		vars := map[string]any{"server": t.Name, "time": end.Local().Format(time.DateTime), "error_msg": resultText(d.Result)}
		if d.Result == "success" {
			template = "AAqXmOqijypwu"
			duration := "未知"
			if !d.Started.IsZero() && !d.Ended.Before(d.Started) {
				seconds := int(d.Ended.Sub(d.Started).Seconds())
				duration = fmt.Sprintf("%d分%d秒", seconds/60, seconds%60)
			}
			vars = map[string]any{"server": t.Name, "duration": duration}
		}
		card, err := s.cards.create(ctx, template, vars)
		if err != nil {
			return err
		}
		if err = s.change(d.ID, func(v *deployment) error { v.ResultCardID = card; return nil }); err != nil {
			return err
		}
		d.ResultCardID = card
	}
	if !d.ResultSent {
		if err := s.cards.send(ctx, t.Group, d.ResultCardID, shortID(d.ID+":result:"+t.Group)); err != nil {
			return err
		}
		if err := s.change(d.ID, func(v *deployment) error { v.ResultSent = true; return nil }); err != nil {
			return err
		}
	}
	if t.Group == "g2" && !d.WorkCopySent {
		if err := s.cards.send(ctx, "work", d.ResultCardID, shortID(d.ID+":result:work")); err != nil {
			return err
		}
		return s.change(d.ID, func(v *deployment) error { v.WorkCopySent = true; return nil })
	}
	return nil
}

func resultText(result string) string {
	switch result {
	case "success":
		return "部署成功"
	case "cancelled":
		return "部署已取消"
	case "failure":
		return "部署失败"
	case "timed_out":
		return "部署超时"
	default:
		return result
	}
}

func workflowTarget(file string) (string, bool) {
	for k, t := range targets {
		if t.File == strings.TrimPrefix(file, ".github/workflows/") {
			return k, true
		}
	}
	return "", false
}
