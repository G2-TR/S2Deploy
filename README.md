# S2Deploy

历史 Work 前端和 G2 服务的飞书自动部署。独立 Go 服务接收 GitLab 推送、发送飞书卡片、处理按钮长连接、触发 GitHub Actions，并把进度和最终结果写回飞书。状态保存在本地 bbolt 数据库。GitHub runner 继续从 GitLab 拉取部署时的分支最新代码。

## 历史部署目标

| 飞书目标 | GitHub 手动入口 | 默认源码分支 |
| --- | --- | --- |
| Work 前端 81、演示、90 | `build.yml`、`build-demo.yml`、`build-work.yml` | `master` |
| G2 CHIS、WORK、TRNG、PLAT、CALL、JZXT | `build-chis.yml`、`build-g2work.yml`、`build-g2trng.yml`、`build-g2plat.yml`、`build-g2call.yml`、`build-g2jzxt.yml` | `prod-merge` |

九个入口调用 `deploy-work-frontend.yml` 或 `deploy-g2.yml`。手动运行时将 `deployment_id` 留空。飞书点击会填入此字段，GitHub 的运行标题为 `deploy-<deployment_id>`。同一目标的 Actions 串行运行。服务器端的目录、备份、解包或 Supervisor 重启及原有验收步骤沿用历史流程。

## 配置和启动

服务镜像为 `ghcr.io/g2-tr/s2deploy`，仅发布 `linux/arm64`，适用于 M 系列 Mac 上的 Docker。推送 `v*` 版本标签会发布版本号、`latest` 和 `sha-<完整提交号>` 三个镜像标签。也可以在 GitHub Actions 的 **Publish deploy service image** 工作流选择 `main` 手动发布 `latest` 和对应提交的镜像。

在服务器上选一个固定目录保存 Compose 文件和凭据。以下命令用于首次安装，假设 Docker 与 Nginx 在同一台宿主机。

```sh
mkdir -p ~/s2deploy && cd ~/s2deploy
curl -fsSLo compose.yaml https://raw.githubusercontent.com/G2-TR/S2Deploy/main/service/compose.yaml
curl -fsSLo .env.example https://raw.githubusercontent.com/G2-TR/S2Deploy/main/service/.env.example
umask 077
cp -n .env.example .env
```

编辑 `.env` 填入以下值。两个群都需要允许原飞书应用发送消息，并保有原卡片模板和更新卡片的权限。

| 配置 | 用途 |
| --- | --- |
| `GITHUB_REPOSITORY` | 工作流仓库，默认 `G2-TR/S2Deploy` |
| `GITHUB_TOKEN` | 对该仓库有 Actions 读写权限的令牌，用于 dispatch、查询运行及作业 |
| `GITHUB_WEBHOOK_SECRET` | 与 GitHub 仓库 Webhook 配置相同的签名密钥，订阅 Workflow runs，使用 JSON |
| `GITLAB_WEBHOOK_TOKEN` | 在历史 Work 和 G2 的 GitLab Push webhook 设置相同的 Secret token |
| `FEISHU_APP_ID`、`FEISHU_APP_SECRET` | 原飞书应用的身份凭据。切换时轮换原本写入博客代码的密钥 |
| `FEISHU_WORK_CHAT_ID`、`FEISHU_G2_CHAT_ID` | Work 群、G2 群的 chat ID，按钮处理会校验来源群和分支 |
| `FEISHU_VERIFICATION_TOKEN` | 使用原 `LarkWebhook` HTTP 验证地址时填写。部署按钮使用飞书长连接 |

```sh
docker compose pull
# 完成下一节的旧服务交接后启动
docker compose up -d
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8096/healthz
docker compose logs --tail=80 deploy-service
```

若 GHCR 提示没有读取权限，先运行 `docker login ghcr.io -u 你的GitHub用户名`，在密码提示处输入有权读取该包的 classic PAT（`read:packages`）。

日常更新时，在同一目录执行 `docker compose pull && docker compose up -d`。要固定版本或回到先前发布的镜像，在 `.env` 设置 `DEPLOY_IMAGE_TAG` 为对应版本标签或 `sha-<完整提交号>`，再执行同一条命令。

返回 200 表示 HTTP 已监听。一个实例独占一个状态库，容器重建时复用 Compose 命名卷 `deploy-data`。备份时先停止服务，再复制完整数据库。直接运行二进制也可以，在 `service/` 执行 `go build -o deploy-service .`，导出同一组环境变量后执行 `./deploy-service`。原生运行默认监听 `127.0.0.1:8096`，默认数据库为工作目录下的 `data/deploy.db`，可通过 `LISTEN_ADDR` 和 `DEPLOY_DB` 覆盖。

## 从 blog-go 切换

1. 先发布本仓库的 9 个工作流入口和 2 个共用工作流到 GitHub 默认分支 `main`。旧服务仍可以使用空 inputs 调用它们。
2. 配好新服务的环境、GitLab token 和 GitHub 签名密钥。先拉取服务镜像。选在历史部署和卡片收尾均已结束时交接，旧服务的 Redis 中间状态不会迁入新数据库。
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

### G2un.work H5

- 发布飞书模板「G2un.work 推送」并授权给部署机器人。在 GitLab 的 G2un.work 项目添加 Push events webhook，地址为部署服务的 `/api/skytek/GitlabWebhookVue3`，Secret token 使用现有 `GITLAB_WEBHOOK_TOKEN`。
- 在工作流仓库添加只读克隆地址 Secret `G2UN_REPO`，以及站点变量 `G2UN_SITE_URL`（格式为 `https://站点域名/G2un/`）。SSH 使用 81 环境已有的 `MAIN_SERVER_HOST`、`MAIN_SERVER_PORT`、`SSH_PASSWORD`。
- 推送卡发往 G2 群。只有 master 的卡片可部署，点击后拉取 GitLab master 最新代码，实际构建的 SHA 记录在 Actions 摘要。工作流更新 `/storage/sites/G2/Page/G2un/` 并保留现有的 `static/config.js`。入口、业务深链、入口 JS 和配置文件通过 HTTP 内容核验后才报告成功。核验失败会恢复上一版。每次更新的上一版位于 Actions 日志中的 `previous` 路径。
