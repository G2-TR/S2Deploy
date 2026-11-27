# S2Work-Deploy

历史 Work 前端和 G2 服务的飞书自动部署。独立 Go 服务接收 GitLab 推送、发送飞书卡片、处理按钮长连接、触发 GitHub Actions，并把进度和最终结果写回飞书。状态保存在本地 bbolt 数据库。GitHub runner 继续从 GitLab 拉取部署时的分支最新代码。

## 历史部署目标

| 飞书目标 | GitHub 手动入口 | 默认源码分支 |
| --- | --- | --- |
| Work 前端 81、演示、90 | `build.yml`、`build-demo.yml`、`build-work.yml` | `master` |
| G2 CHIS、WORK、TRNG、PLAT、CALL、JZXT | `build-chis.yml`、`build-g2work.yml`、`build-g2trng.yml`、`build-g2plat.yml`、`build-g2call.yml`、`build-g2jzxt.yml` | `prod-merge` |

九个入口调用 `deploy-work-frontend.yml` 或 `deploy-g2.yml`。手动运行时将 `deployment_id` 留空。飞书点击会填入此字段，GitHub 的运行标题为 `deploy-<deployment_id>`。同一目标的 Actions 串行运行。服务器端的目录、备份、解包或 Supervisor 重启及原有验收步骤沿用历史流程。

## 配置和启动

下面的命令从本仓库根目录执行。Compose 适用于 Nginx 和 Docker 位于同一主机的部署方式。

```sh
cd service
cp .env.example .env
chmod 600 .env
```

编辑 `.env` 填入以下值。两个群都需要允许原飞书应用发送消息，并保有原卡片模板和更新卡片的权限。

| 配置 | 用途 |
| --- | --- |
| `GITHUB_REPOSITORY` | 工作流仓库，默认 `hamster1963/S2Work-Deploy` |
| `GITHUB_TOKEN` | 对该仓库有 Actions 读写权限的令牌，用于 dispatch、查询运行及作业 |
| `GITHUB_WEBHOOK_SECRET` | 与 GitHub 仓库 Webhook 配置相同的签名密钥，订阅 Workflow runs，使用 JSON |
| `GITLAB_WEBHOOK_TOKEN` | 在历史 Work 和 G2 的 GitLab Push webhook 设置相同的 Secret token |
| `FEISHU_APP_ID`、`FEISHU_APP_SECRET` | 原飞书应用的身份凭据。切换时轮换原本写入博客代码的密钥 |
| `FEISHU_WORK_CHAT_ID`、`FEISHU_G2_CHAT_ID` | Work 群、G2 群的 chat ID，按钮处理会校验来源群和分支 |
| `FEISHU_VERIFICATION_TOKEN` | 使用原 `LarkWebhook` HTTP 验证地址时填写。部署按钮使用飞书长连接 |

```sh
docker compose build
# 完成下一节的旧服务交接后启动
docker compose up -d
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8096/healthz
docker compose logs --tail=80 deploy-service
```

返回 200 表示 HTTP 已监听。一个实例独占一个状态库，容器重建时复用 Compose 命名卷 `deploy-data`。备份时先停止服务，再复制完整数据库。直接运行二进制也可以，在 `service/` 执行 `go build -o deploy-service .`，导出同一组环境变量后执行 `./deploy-service`。原生运行默认监听 `127.0.0.1:8096`，默认数据库为工作目录下的 `data/deploy.db`，可通过 `LISTEN_ADDR` 和 `DEPLOY_DB` 覆盖。

## 从 blog-go 切换

1. 先发布本仓库的 9 个工作流入口和 2 个共用工作流到 GitHub 默认分支 `main`。旧服务仍可以使用空 inputs 调用它们。
2. 配好新服务的环境、GitLab token 和 GitHub 签名密钥。先构建服务。选在历史部署和卡片收尾均已结束时交接，旧服务的 Redis 中间状态不会迁入新数据库。
3. 部署已移除 Skytek 路由和飞书长连接的 blog-go 构建，随后启动新服务。确保同一飞书应用只有新服务消费按钮事件。博客的构建 ID 和 SSE 接口继续走博客。
4. 在现有 webhook 域名的 Nginx server 中加入下列 location。保留 `proxy_pass` 的写法以转发完整 URI。检查 `nginx -t` 成功后重载。如果 Nginx 在容器中，请将上游改为其网络可达的服务地址。

   ```nginx
   location ^~ /api/skytek/ {
       proxy_pass http://127.0.0.1:8096;
   }
   ```

5. GitLab 推送沿用 `POST /api/skytek/GitlabWebhook` 和 `POST /api/skytek/GitlabWebhookG2`；GitHub 沿用 `POST /api/skytek/WorkflowWebhook`。先检查一次 webhook delivery，再从 81 群卡片点一次部署，核对 Actions 运行标题、进度卡链接和结果卡。G2 进度发到 G2 群，结果发到 G2 群与 Work 群。手动运行也会在收到完成事件后发送结果卡。

如需回切，先等待新服务中的部署和卡片收尾完成，停止新服务，再将上游切回旧博客版本并恢复其飞书连接。若已轮换飞书凭据，回切版本必须使用有效凭据。

## 状态恢复

点击事件、目标占用、GitHub run ID、卡片 ID 和发送进度均持久化。收到完成 webhook 或轮询到完成后，服务先保存结果并释放目标占用，再更新原进度卡和发送结果卡。飞书失败后会在后续轮询中重试，服务重启会继续处理。

若 dispatch 响应丢失，服务只按该工作流中的精确运行标题查找，避免自动重发导致重复发布。极少数情况下，服务可能在记录已发送意图后、实际发出 HTTP 请求前退出。此时目标会一直等待确认。可按下列步骤人工解除：

```sh
docker compose stop deploy-service
docker compose run --rm --no-deps deploy-service pending
# 在 GitHub Actions 中核对对应目标，确认没有 deploy-<ID> 运行后执行
docker compose run --rm --no-deps deploy-service close-unconfirmed DEPLOYMENT_ID
docker compose up -d
```

该命令只接受等待 dispatch 确认、尚无 run ID 和最终结果的请求。恢复服务后会向飞书发送关闭结果。使用原生二进制时，停止服务后以同一 `DEPLOY_DB` 运行相同的子命令。

## 本地验证

进入 `service/` 执行 `go test -race ./...`。测试使用临时状态库、模拟 GitHub HTTP 端点和飞书卡片，覆盖并发点击、响应丢失、签名回调、卡片更新失败后重启恢复以及手动运行的双群结果通知。
