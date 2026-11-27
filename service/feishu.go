package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	cardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

var notifyTemplate = map[string]string{"work": "AAqXqP6SMSpxU", "g2": "AAqemMDkkGeNV"}

type feishu struct {
	client *lark.Client
	chats  map[string]string
}

func jsonString(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return string(b)
}

func shortID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

func (f feishu) create(ctx context.Context, template string, vars map[string]any) (string, error) {
	body := cardkit.NewCreateCardReqBodyBuilder().Type("template").Data(jsonString(map[string]any{"template_id": template, "template_variable": vars})).Build()
	r, e := f.client.Cardkit.V1.Card.Create(ctx, cardkit.NewCreateCardReqBuilder().Body(body).Build())
	if e != nil {
		return "", e
	}
	if !r.Success() || r.Data == nil || r.Data.CardId == nil {
		return "", fmt.Errorf("Feishu create card: code %d", r.Code)
	}
	return *r.Data.CardId, nil
}

func (f feishu) send(ctx context.Context, group, card, id string) error {
	body := im.NewCreateMessageReqBodyBuilder().ReceiveId(f.chats[group]).MsgType("interactive").Content(jsonString(map[string]any{"type": "card", "data": map[string]string{"card_id": card}}))
	if id != "" {
		body.Uuid(id)
	}
	r, e := f.client.Im.V1.Message.Create(ctx, im.NewCreateMessageReqBuilder().ReceiveIdType("chat_id").Body(body.Build()).Build())
	if e != nil {
		return e
	}
	if !r.Success() {
		return fmt.Errorf("Feishu send: code %d", r.Code)
	}
	return nil
}

func (f feishu) patch(ctx context.Context, card string, seq int, text string) error {
	body := cardkit.NewPatchCardElementReqBodyBuilder().PartialElement(jsonString(map[string]string{"content": text})).Sequence(seq).Build()
	r, e := f.client.Cardkit.V1.CardElement.Patch(ctx, cardkit.NewPatchCardElementReqBuilder().CardId(card).ElementId("stream_update_sect").Body(body).Build())
	if e != nil {
		return e
	}
	if !r.Success() {
		return fmt.Errorf("Feishu patch: code %d", r.Code)
	}
	return nil
}

func toast(text, kind string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: kind, Content: text}}
}

func (s *service) button(chats map[string]string) func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	return func(ctx context.Context, e *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		if e == nil || e.Event == nil || e.Event.Action == nil || e.Event.Context == nil || e.EventV2Base == nil || e.EventV2Base.Header == nil || e.EventV2Base.Header.EventID == "" {
			return toast("无效的卡片事件", "error"), nil
		}
		values := e.Event.Action.Value
		key, _ := values["server"].(string)
		branch := "refs/heads/master"
		group := "work"
		kind, _ := values["type"].(string)
		if kind == "G2" {
			group = "g2"
			name, _ := values["service"].(string)
			name = strings.TrimPrefix(strings.ToLower(name), "g2-")
			if name == "" || name == "g2chis" {
				name = "chis"
			}
			key = "g2-" + name
			branch = "refs/heads/prod-merge"
		}
		t, ok := targets[key]
		if !ok || t.Group != group || e.Event.Context.OpenChatID != chats[t.Group] || values["branch"] != branch {
			return toast("部署目标、群或分支不匹配", "error"), nil
		}
		id := shortID(e.EventV2Base.Header.EventID)
		if err := s.request(e.EventV2Base.Header.EventID, id, key); err != nil {
			if err == busy {
				return toast(busy.Error(), "warning"), nil
			}
			return nil, err
		}
		s.signal()
		return toast("已受理"+t.Name, "success"), nil
	}
}

func pushVars(ref, message, user, group string) map[string]any {
	allowed := "refs/heads/master"
	if group == "g2" {
		allowed = "refs/heads/prod-merge"
	}
	return map[string]any{"commit_message": message, "user": user, "branch": ref, "time": time.Now().Format("2006-01-02 15:04:05"), "deploy_disable": ref != allowed}
}
