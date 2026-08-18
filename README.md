# GPUFlow

GPUFlow 是一个轻量的 BYOC AI 批任务调度器 MVP。用户连接自己的公有云或自建 GPU，GPUFlow 负责统一接收任务、选择节点、执行、监控和失败重试；算力费用仍由用户直接支付给供应商。

## 当前能力

- 单个 Go 二进制同时包含控制面、Agent 和 CLI；
- 节点注册、心跳、离线判断和安全删除；
- 节点存在已分配或运行中任务时，前端和服务端都会拒绝删除；
- 按 GPU 数量、显存、型号、供应商、资源池、标签和最高时价过滤；
- `lowest_cost` 与 `most_vram` 调度策略；
- Docker 任务执行、超时控制、日志回传和自动重试；
- JSON 文件持久化，适合单机 MVP 和轻量私有化；
- Bearer Token API 鉴权；
- 内置响应式 Web 控制台，可查看概览、节点、任务并提交任务；
- Web 控制台可上传单个 `.py`/`.sh` 脚本，选择 Python、CUDA 或 PyTorch 环境并自动构建任务镜像；
- Docker Compose 一键启动演示环境。
- GitHub Actions 自动发布 `linux/amd64`、`linux/arm64` 多架构容器镜像。

当前版本只实现本地 Docker Agent。阿里云和腾讯云应作为后续 Provider Adapter 接入，而不是把厂商 SDK 写进调度核心。

控制面启动后访问 `http://localhost:8080` 即可打开 Web 控制台。若配置了 `GPUFLOW_TOKEN`，页面会提示输入，Token 只保存在当前浏览器会话中。

Linux / Docker 接入页会显示可编辑的 Agent 镜像地址。控制端可通过 `GPUFLOW_AGENT_IMAGE` 设置默认值；正式发布后建议配置为固定版本，例如 `ghcr.io/<owner>/<repository>:0.1.0`。

前端源码位于 `web/`，构建结果嵌入 Go 二进制：

```powershell
cd web
npm install
npm run build
cd ..
go build -o bin/gpuflow.exe ./cmd/gpuflow
```

## 快速体验

要求安装 Docker。复制环境变量示例并启动：

```powershell
Copy-Item .env.example .env
docker compose up --build -d
```

默认不会创建任何算力节点。需要演示 Agent 时显式启用：

```powershell
docker compose --profile demo up --build -d
```

如果本机安装了 Go，可以构建 CLI：

```powershell
go build -o bin/gpuflow.exe ./cmd/gpuflow
```

提交示例任务：

```powershell
$headers = @{ Authorization = 'Bearer development-token'; 'Content-Type' = 'application/json' }
Invoke-RestMethod -Method Post -Uri http://localhost:8080/v1/jobs -Headers $headers -InFile examples/job.json
Invoke-RestMethod -Uri http://localhost:8080/v1/jobs -Headers $headers
Invoke-RestMethod -Uri http://localhost:8080/v1/nodes -Headers $headers
```

演示任务不要求 GPU。生产节点把 `GPUFLOW_GPU_COUNT`、`GPUFLOW_VRAM_GB` 和 `GPUFLOW_GPU_MODEL` 设置为真实值；GPU Docker 任务还要求宿主机安装 NVIDIA Container Toolkit。

## 从脚本构建任务镜像

Web 控制台的“任务镜像”页面提供单文件构建器：

1. 上传不超过 5 MB 的 `.py` 或 `.sh` 脚本；
2. 选择 Shell、Python 3.12、CUDA 12.0 或 PyTorch CUDA 运行环境；
3. Python/PyTorch 环境可填写 `requirements.txt` 内容；
4. 查看异步构建状态和 Docker 日志；
5. 构建成功后点击“使用此镜像提交任务”，镜像和入口命令会自动带入任务表单。

该功能在控制端调用本机 `docker build`，因此控制端必须能访问 Docker Engine。构建记录暂存在内存中，控制端重启后列表会清空，但已构建的镜像仍保留在 Docker 中。

当前版本适合控制端与 Agent 共用同一个 Docker Engine 的单机环境。远程 Agent 无法直接访问控制端本地镜像；多节点部署需要把镜像推送到 GHCR、Harbor 等共享仓库，再用完整仓库地址提交任务。

## 发布容器镜像

仓库内置 `.github/workflows/container-image.yml`。向 GitHub 推送符合 `v*.*.*` 的版本标签后，GitHub Actions 会构建前端和 Go 程序，并将多架构镜像发布到 GitHub Container Registry：

```text
ghcr.io/<owner>/<repository>:<version>
```

首次发布流程：

```bash
git tag v0.1.0
git push origin v0.1.0
```

工作流自动生成以下标签：

- `0.1.0`：固定版本；
- `0.1`：同一小版本的最新补丁；
- `latest`：最新稳定版本；
- `sha-xxxxxxx`：对应源码提交；
- `edge`：从 GitHub Actions 手动运行工作流时生成。

工作流使用仓库自带的 `GITHUB_TOKEN` 推送 GHCR，无须额外配置 Registry 密码。第一次发布后，需要在 GitHub Packages 设置中确认镜像包为 Public，开源用户才能匿名拉取。

拉取镜像时，建议生产环境使用固定版本，不要依赖 `latest`：

```bash
docker pull ghcr.io/<owner>/<repository>:0.1.0
```

使用容器运行远程 Agent：

```bash
docker run -d \
  --name gpuflow-agent \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  ghcr.io/<owner>/<repository>:0.1.0 agent \
  -server "http://控制端IP:18080" \
  -id "linux-rtx4090-01" \
  -name "linux-rtx4090-01" \
  -provider local \
  -pool default \
  -gpu-model RTX-4090 \
  -gpu-count 1 \
  -vram 24 \
  -hourly-price 0 \
  -executor docker
```

Agent 容器通过 `/var/run/docker.sock` 调用宿主机 Docker。执行 GPU 任务时，宿主机必须已安装 NVIDIA 驱动和 NVIDIA Container Toolkit。Docker Socket 拥有很高的宿主机权限，只能运行可信镜像并连接可信控制端。

## 命令

```text
gpuflow server [-addr :8080] [-data ./data/state.json]
gpuflow agent  [-server URL] [-pool default] [-executor docker|mock]
gpuflow submit -file job.json
gpuflow jobs
gpuflow nodes
gpuflow get JOB_ID
```

所有命令支持 `GPUFLOW_SERVER` 和 `GPUFLOW_TOKEN` 环境变量。

## Pro 与私有化演进

建议坚持 Open Core 边界：

| 开源核心 | Pro/企业功能 |
|---|---|
| CLI、Agent、任务格式 | 实时价格与库存数据 |
| 本地 Docker Provider | 阿里云/腾讯云托管适配器 |
| 基础队列与重试 | 高级成本、截止时间和可靠性策略 |
| 单租户文件存储 | PostgreSQL、多租户、RBAC、审计日志 |
| 基础 API 与 Web 控制台 | 团队工作台、预算、告警与报表 |
| 社区支持 | 私有化安装器、升级服务和 SLA |

私有化版本可以将控制面、数据库和 Agent 全部部署在客户网络中，授权文件离线校验。不要让开源 Agent 依赖收费许可证，否则会削弱 GitHub 获客效果；收费点应放在持续数据、高级调度、团队治理和运维服务上。

商业扩展维护在独立私有仓库中，通过公开的 `pkg/edition` 和 `pkg/platform` 组合开源核心，不会提交到本开源仓库。

## 生产化前必须补齐

- mTLS 或短期 Agent 凭证，替代共享 Token；
- PostgreSQL 与可靠队列，替代 JSON 状态文件；
- 租户隔离、RBAC、审计日志和密钥托管方案；
- 制品/检查点存储和跨资源池数据传输成本模型；
- 任务取消、Agent 宕机回收和租约机制；
- Provider API 限流、幂等和账单对账；
- Agent 沙箱与镜像白名单。

Agent 能在宿主机启动容器，属于高权限组件。只应连接可信控制面，并部署在专用执行节点上；不要在个人电脑或包含敏感工作负载的主机上运行未经审核的任务。
