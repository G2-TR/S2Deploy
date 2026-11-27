package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

func main() {
	if err := runCommand(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func databasePath() string {
	if name := os.Getenv("DEPLOY_DB"); name != "" {
		return name
	}
	return "data/deploy.db"
}

// Recovery commands use the same exclusive database lock as the worker.
func runCommand(args []string) error {
	if len(args) == 0 {
		return serve()
	}
	if !(len(args) == 1 && args[0] == "pending" || len(args) == 2 && args[0] == "close-unconfirmed") {
		return errors.New("usage: deploy-service [pending | close-unconfirmed DEPLOYMENT_ID]")
	}
	filename := databasePath()
	if _, err := os.Stat(filename); err != nil {
		return err
	}
	store, err := openStore(filename)
	if err != nil {
		return err
	}
	defer store.db.Close()
	if args[0] == "pending" {
		items, err := store.list()
		if err != nil {
			return err
		}
		for _, d := range items {
			fmt.Printf("%s  %s  %s  run=%d  result=%q\n", d.ID, d.Target, d.Phase, d.RunID, d.Result)
		}
		return nil
	}
	return store.change(args[1], func(d *deployment) error {
		if d.Phase != "dispatched" || d.RunID != 0 || d.Result != "" {
			return errors.New("deployment is not awaiting dispatch confirmation")
		}
		d.Result = "人工确认 GitHub 未创建运行，已关闭请求"
		return nil
	})
}

func serve() error {
	for _, key := range []string{"GITHUB_TOKEN", "GITHUB_WEBHOOK_SECRET", "GITLAB_WEBHOOK_TOKEN", "FEISHU_APP_ID", "FEISHU_APP_SECRET", "FEISHU_WORK_CHAT_ID", "FEISHU_G2_CHAT_ID"} {
		if os.Getenv(key) == "" {
			return fmt.Errorf("missing %s", key)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	filename := databasePath()
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return err
	}
	db, err := openStore(filename)
	if err != nil {
		return err
	}
	defer db.db.Close()
	repo := os.Getenv("GITHUB_REPOSITORY")
	if repo == "" {
		repo = "G2-TR/S2Deploy"
	}
	appID, secret := os.Getenv("FEISHU_APP_ID"), os.Getenv("FEISHU_APP_SECRET")
	chats := map[string]string{"work": os.Getenv("FEISHU_WORK_CHAT_ID"), "g2": os.Getenv("FEISHU_G2_CHAT_ID")}
	client := lark.NewClient(appID, secret, lark.WithReqTimeout(10*time.Second))
	s := &service{
		store: db, repo: repo,
		github: gitHub{
			base:  "https://api.github.com/repos/" + repo,
			token: os.Getenv("GITHUB_TOKEN"),
			http:  &http.Client{Timeout: 12 * time.Second},
		},
		cards: feishu{client, chats},
		wake:  make(chan struct{}, 1),
	}
	h := hooks{
		s:            s,
		githubSecret: os.Getenv("GITHUB_WEBHOOK_SECRET"),
		gitlabToken:  os.Getenv("GITLAB_WEBHOOK_TOKEN"),
		larkToken:    os.Getenv("FEISHU_VERIFICATION_TOKEN"),
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8096"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second}
	eventHandler := dispatcher.NewEventDispatcher("", "").OnP2CardActionTrigger(s.button(chats))
	ws := larkws.NewClient(appID, secret, larkws.WithEventHandler(eventHandler))
	errs := make(chan error, 2)
	go func() { errs <- ws.Start(ctx) }()
	go func() { errs <- srv.Serve(listener) }()
	done := make(chan struct{})
	go func() { defer close(done); s.work(ctx) }()
	log.Printf("deploy service listening on %s", listener.Addr())
	select {
	case err = <-errs:
	case <-ctx.Done():
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
