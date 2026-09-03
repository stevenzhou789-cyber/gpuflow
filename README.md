# GPUFlow

轻量级 BYOC（Bring Your Own Compute）GPU 批任务调度器。

GPUFlow 把分散在本地工作站、实验室服务器和自建机房中的 GPU 节点接入同一个控制面，统一完成任务提交、资源匹配、执行监控、失败重试与日志查看。算力仍由使用者自己提供，GPUFlow 不转售算力，也不介入云厂商计费。

> 当前状态：社区稳定维护版。节点接入、批任务调度、日志、重试、产物和持久化已形成完整闭环；后续以小步维护、可靠性修复和必要底层能力为主，企业治理能力不会下放到本仓库。

> **从一台机器跑通，到一组 GPU 高效协作：** Community 负责把批任务闭环交到你手里；当节点增多、团队开始共用算力时，[GPUFlow Enterprise](#从开源验证到企业落地) 可进一步提供 GPU 粒度并发调度、内置镜像分发、角色权限与审计能力。

## 为什么使用 GPUFlow

- **统一入口**：不必分别登录每台 GPU 主机，任务统一从 Web 控制台或 CLI 提交。
- **按资源调度**：根据 GPU 数量、显存、资源池和参考单价选择可用节点。
- **任务级可靠性**：支持超时、失败重试、状态跟踪和运行日志。
- **BYOC 模式**：节点和云账号归用户所有，费用直接由用户承担。
- **轻量部署**：控制面、Agent 和 CLI 由同一个 Go 程序提供。

```mermaid
flowchart LR
    U["用户 / Web / CLI"] --> C["GPUFlow 控制面"]
    C --> A1["Agent · GPU 节点 A"]
    C --> A2["Agent · GPU 节点 B"]
    A1 --> D1["Docker GPU 任务"]
    A2 --> D2["Docker GPU 任务"]
```

## 已有能力

- 节点注册、心跳、在线状态、安全删除、搜索与分页
- 运行任务中的节点禁止删除
- `lowest_cost`、`most_vram` 调度策略
- GPU 数量、最低显存和资源池约束
- Agent 启动时自动识别 GPU 型号、数量和单卡显存汇总，不保留手工 GPU 容量覆盖
- Docker 容器执行、超时控制和失败重试
- 任务状态、输出与日志查看
- 任务停止、重跑、删除、搜索、过滤与分页
- 任务产物自动归档与下载
- Bearer Token API 鉴权
- 任务、节点和任务镜像记录使用 MySQL 持久化
- 响应式 Web 控制台
- 从 Python 或 Shell 脚本构建任务镜像，并支持搜索、分页和删除
- Windows、Linux 和 Docker Agent 接入指引

## 快速开始

### 环境要求

- Docker
- Docker Compose

### 1. 配置并启动服务

首次启动前复制环境变量示例，并修改其中的 Token、MySQL 密码和 MinIO 密码：

```bash
cp .env.example .env
docker compose up --build -d
```

Windows PowerShell：

```powershell
Copy-Item .env.example .env
docker compose up --build -d
```

Compose 会把当前源码构建为 `gpuflow:local`。控制面默认下发固定 Digest 的 Debian/glibc Probe 镜像，在线计算节点按需自动拉取，不需要重复配置镜像地址。默认只启动 GPUFlow 控制面、MySQL 和 MinIO，不会自动启动或注册算力节点，也不会自动提交任务。打开 [http://localhost:18080](http://localhost:18080)，使用 `.env` 中的 `GPUFLOW_TOKEN` 登录。

确认三个服务正常：

```bash
docker compose ps
```

### 2. 接入算力节点

在 Web 控制台进入 **节点 → 接入算力节点**，填写节点信息，并在目标机器执行页面生成的 Agent 命令。节点开始持续发送心跳后，页面才会显示 `ONLINE`；仅启动控制面不会产生在线节点。

Agent 启动时自动读取节点的 GPU 型号、数量和单卡显存汇总；未检测到 NVIDIA GPU 的原生 Agent 会按 CPU 节点注册。创建任务时仍需按任务实际需要填写 GPU 数量和最低显存，否则任务可能无法匹配节点。

### 3. 构建并提交第一个任务

在 **任务镜像 → 上传任务脚本** 中选择 [examples/quick-smoke.sh](examples/quick-smoke.sh)，运行环境选择 **Shell**，完成镜像构建后点击 **使用此镜像提交任务**。CPU 节点测试时将任务的 GPU 数量和最低显存都设为 `0`，并确保任务资源池与节点一致或留空。

任务完成后可以查看日志，并下载包含 `result.txt` 的 `artifacts.tar.gz`。更详细的节点参数和脚本构建说明见后文“接入真实 GPU 节点”和“从脚本构建任务镜像”。

停止服务但保留数据：

```bash
docker compose down
```

## 一体化部署与数据存储

一体化部署会分别启动 GPUFlow 控制端、MySQL 和 MinIO 三个容器。MySQL 保存任务、节点和任务镜像记录，MinIO 保存任务产物；两者各自使用独立 Docker Volume，重新创建 GPUFlow 容器不会丢失这些数据。

控制端会幂等创建 `jobs`、`nodes`、`task_images` 表与 `gpuflow-artifacts` 存储桶，不会清空已有数据。MinIO 控制台默认位于 [http://127.0.0.1:9001](http://127.0.0.1:9001)。查看运行日志：

```bash
docker compose logs -f control-plane mysql minio
```

不要随意添加 `-v`，否则会删除 MySQL 和 MinIO 数据卷。

任务脚本只需把需要保留的文件写入 `$GPUFLOW_ARTIFACT_DIR`。目录中存在文件时，Agent 会在任务结束后生成 `artifacts.tar.gz` 并上传；在任务队列点击任务即可通过“下载产物”按钮获取。产物打包或上传失败会写入任务输出作为 warning，不改变任务程序本身的成功或失败状态。

使用已有 MySQL 和 MinIO/S3 时，为控制端设置：

```env
GPUFLOW_MYSQL_DSN=gpuflow:密码@tcp(mysql-host:3306)/gpuflow?parseTime=true&charset=utf8mb4
GPUFLOW_S3_ENDPOINT=minio-host:9000
GPUFLOW_S3_ACCESS_KEY=gpuflow
GPUFLOW_S3_SECRET_KEY=替换为实际密码
GPUFLOW_S3_BUCKET=gpuflow-artifacts
GPUFLOW_S3_REGION=
GPUFLOW_S3_USE_SSL=false
```

使用外部服务时，应先创建 MySQL 数据库和账号，并授予建表及读写权限；S3 凭证需要具备目标存储桶的读、写、列举和删除权限，存储桶不存在时还需要创建权限。

MySQL 和 MinIO/S3 都是必需依赖。MySQL DSN、S3 endpoint 或 S3 access/secret key 缺失，或者服务连接失败时，控制面会拒绝启动。

## 原地升级与回滚

正式部署使用不可变的 `v*` 镜像后，可以在控制面服务器执行一条命令完成原地升级：

```bash
./scripts/upgrade.sh v1.0.1
```

脚本会先确认目标镜像存在，把 `.env`、`compose.yaml` 和 MySQL 备份到 `.gpuflow/backups/`，然后替换控制面容器并检查 `/healthz`。MySQL 与 MinIO Volume 不会被删除。健康检查失败时会自动切回原控制面镜像，但不会自动覆盖数据库。

默认镜像仓库为 `ghcr.io/stevenzhou789-cyber/gpuflow`。使用企业私有仓库时可以指定：

```bash
./scripts/upgrade.sh v1.0.1 \
  --image-repository harbor.example.com/gpuflow/gpuflow
```

### 集中升级 Agent

要让控制面服务器逐台升级 Linux/Docker Agent，节点应使用 [deploy/agent/compose.yaml](deploy/agent/compose.yaml) 管理，而不是保留一条无法可靠还原参数的临时 `docker run` 命令。在每个节点初始化一次：

```bash
sudo mkdir -p /opt/gpuflow-agent
sudo cp deploy/agent/compose.yaml deploy/agent/.env.example /opt/gpuflow-agent/
sudo chown -R "$USER":"$USER" /opt/gpuflow-agent
cd /opt/gpuflow-agent
sudo mv .env.example .env
# 编辑 .env 中的控制面地址、Token 和节点 ID；GPU 容量由 Agent 自动识别
docker compose up -d
```

在控制面复制节点清单并填写 SSH 地址：

```bash
cp scripts/agents.conf.example scripts/agents.conf
```

清单格式为 `SSH目标|Agent安装目录`：

```text
ops@gpu-01|/opt/gpuflow-agent
ops@gpu-02|/opt/gpuflow-agent
```

SSH 密钥、堡垒机和 `ProxyJump` 应配置在控制面服务器的 `~/.ssh/config`，不要把密码或私钥写进清单。存在 `scripts/agents.conf` 时，`upgrade.sh` 会在控制面升级成功后逐台升级 Agent；未配置清单时只升级控制面。节点上仍有带 `gpuflow.job` 标签的运行任务时，脚本会停止并保留该节点当前版本，不会中断任务。处理完任务后重新执行同一升级命令即可。

只升级控制面或只升级 Agent：

```bash
./scripts/upgrade.sh v1.0.1 --skip-agents
./scripts/upgrade-agents.sh v1.0.1
```

程序版本回滚同样从控制面执行：

```bash
./scripts/rollback.sh stable
```

回滚脚本只切换控制面和 Agent 镜像，不恢复 MySQL 或 MinIO 数据。跨越不兼容数据库迁移之前，应先根据对应版本的升级说明判断旧程序是否能够读取当前数据库；需要恢复数据库备份时必须单独安排维护窗口，并接受备份时间点之后的数据会丢失。

脚本需要 Linux、Bash、Docker Compose v2；批量升级还需要控制面能够使用 SSH 公钥登录节点。交付 ZIP 解压后如果脚本没有执行权限，先运行：

```bash
chmod +x scripts/*.sh
```

## 接入真实 GPU 节点

目标主机需要具备：

- Docker
- NVIDIA GPU 驱动
- NVIDIA Container Toolkit（运行 GPU 容器时需要）
- 能够访问 GPUFlow 控制面的网络

启动控制面后，在 Web 控制台进入 **节点 → 接入算力节点**。选择目标系统并填写节点信息，页面会生成可直接复制的 Agent 命令。

接入时请注意：

- **控制端地址**必须是目标主机能够访问的地址。
- 本机测试可以使用 `http://127.0.0.1:18080`。
- 远程节点应改为控制面所在机器的局域网 IP、域名或 HTTPS 地址。
- **节点标识**必须唯一，并在节点重启后保持不变，例如 `lab-rtx4090-01`。
- **调度参考单价**的单位为人民币元/GPU/小时，仅用于节点排序；本机测试可填写 `0`。

控制面可通过 `GPUFLOW_PUBLIC_URL` 设置页面默认生成的远程访问地址：

```env
GPUFLOW_PUBLIC_URL=http://127.0.0.1:18080
```

默认值适合本机体验。部署到其他机器后，请由部署者改成节点实际可访问的地址。

### 获取 GPUFlow Agent

GPUFlow 不区分“CLI 程序”和“Agent 程序”。控制端、Agent 和命令行工具都由同一个 `gpuflow` 可执行文件提供，通过 `server`、`agent`、`submit` 等子命令选择运行模式。

Windows x64 节点可以从 [GPUFlow Releases](https://github.com/stevenzhou789-cyber/gpuflow/releases) 下载所需 `v*` 版本的 `gpuflow-windows-amd64.zip`，解压后得到 `gpuflow.exe`。也可以在源码根目录自行构建：

```powershell
New-Item -ItemType Directory -Force .\bin
go build -trimpath -o .\bin\gpuflow.exe .\cmd\gpuflow
```

Linux 节点从源码构建：

```bash
mkdir -p bin
go build -trimpath -o ./bin/gpuflow ./cmd/gpuflow
```

Windows Agent 的启动方式如下；实际使用时，应优先复制控制台生成的命令，并确保 Token 与控制端 `.env` 中的 `GPUFLOW_TOKEN` 完全一致：

```powershell
.\gpuflow.exe agent `
  -server "http://127.0.0.1:18080" `
  -token "与 .env 中 GPUFLOW_TOKEN 相同的值" `
  -id "local-gpu-01" `
  -name "local-gpu-01" `
  -provider local `
  -pool local `
  -hourly-price 0 `
  -executor docker
```

可执行文件路径应写成 `.\gpuflow.exe`，不能写成 `.\gpuflow\.exe`。`-server` 后面必须是纯 URL；远程节点不能使用 `127.0.0.1`，应改为控制端所在机器的局域网 IP、域名或公网 HTTPS 地址。

### 不同部署环境的程序与镜像获取

`gpuflow` 系统镜像既可以运行控制端，也可以通过 `agent` 子命令运行容器化 Agent。应根据部署环境选择获取方式：

| 部署环境 | 推荐获取方式 | 使用的程序或镜像 |
| --- | --- | --- |
| 本机源码体验 | 执行 `docker compose up --build -d`，由 Compose 从当前源码构建 | `gpuflow:local` |
| 可访问互联网的 Linux 节点 | 从 GHCR 拉取明确的版本镜像 | `ghcr.io/stevenzhou789-cyber/gpuflow:stable` |
| 多节点或企业内网 | 将同一版本的 GHCR 镜像同步到所有节点可访问的 Harbor 或其他私有仓库 | `harbor.example.com/gpuflow/gpuflow:stable` |
| 无法访问外网的离线节点 | 在联网机器拉取对应架构镜像，使用 `docker save` 导出后传入离线环境并执行 `docker load` | 导入后的本地镜像标签 |
| Windows 原生 Agent | 下载对应 `v*` Release 中的 Windows 压缩包，或使用 Go 从源码构建 | `gpuflow.exe`，不需要 Agent 镜像 |
| 修改源码后的自定义部署 | 在仓库根目录执行 `docker build` | 自定义镜像标签 |

可联网的 Linux/Docker 环境直接拉取：

```bash
docker pull ghcr.io/stevenzhou789-cyber/gpuflow:stable
```

同步到私有仓库：

```bash
docker pull ghcr.io/stevenzhou789-cyber/gpuflow:stable
docker tag ghcr.io/stevenzhou789-cyber/gpuflow:stable \
  harbor.example.com/gpuflow/gpuflow:stable
docker push harbor.example.com/gpuflow/gpuflow:stable
```

同步完成后，在控制端 `.env` 中设置镜像地址，Web 控制台生成的 Docker Agent 命令就会使用该地址：

```env
GPUFLOW_AGENT_IMAGE=harbor.example.com/gpuflow/gpuflow:stable
```

离线环境应明确选择目标 CPU 架构。以下示例导出 Linux amd64 镜像：

```bash
# 在可联网机器执行
docker pull --platform linux/amd64 ghcr.io/stevenzhou789-cyber/gpuflow:stable
docker save -o gpuflow-stable-linux-amd64.tar \
  ghcr.io/stevenzhou789-cyber/gpuflow:stable

# 将 tar 文件复制到离线节点后执行
docker load -i gpuflow-stable-linux-amd64.tar
```

ARM64 节点将 `linux/amd64` 改为 `linux/arm64`。如果修改过源码，可以在仓库根目录构建本地镜像：

```bash
docker build -t gpuflow:local .
```

这里的 `gpuflow` 系统镜像与用户任务镜像不是同一个概念。系统镜像用于运行控制端或 Agent；任务镜像用于执行用户脚本。Community 在控制端本地构建任务镜像，多机部署时还需要把任务镜像推送到所有 Agent 都能访问的共享仓库，或者预先在每台节点上加载相同标签的任务镜像，否则远程 Agent 无法启动任务容器。

### Docker Agent 的运行方式

Agent 需要访问宿主机 Docker，因此容器启动时通常要挂载 Docker Socket：

```bash
docker run -d \
  --name gpuflow-agent \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /var/lib/gpuflow/artifacts:/var/lib/gpuflow/artifacts \
  -e GPUFLOW_ARTIFACT_WORKDIR=/var/lib/gpuflow/artifacts \
  ghcr.io/stevenzhou789-cyber/gpuflow:stable agent \
  -server "http://control-plane.example.com:8080" \
  -token "replace-with-your-token" \
  -id "lab-gpu-01" \
  -name "lab-gpu-01"
```

Agent 自动采用控制面下发的独立 glibc Probe 镜像：Windows/Linux 原生 Agent 先读取宿主机 `nvidia-smi`，容器 Agent 无法读取宿主机命令时会通过 Docker Socket 自动拉取并运行 Probe。探测或 Docker Runtime 暂时失败时，Agent 仍会以 `DEGRADED` 注册并周期重试，不会在注册前退出；同一节点 DEGRADED 重连时控制面会保留最后一次已知 GPU 清单，恢复后自动转为 `HEALTHY`。

正式节点应使用明确的 `v*` 版本、`sha-*` 镜像或 Digest；与控制端共用 Docker 的本地开发节点可以使用 Compose 自动构建的 `gpuflow:local`。产物工作目录必须以相同绝对路径挂载到 Agent 容器；Agent 直接作为宿主机进程运行时不需要设置该目录。实际接入时，建议优先复制控制台根据当前配置生成的完整命令。

Agent 应使用稳定且唯一的 `-id`。任务执行期间 Agent 会持续发送心跳。同一 ID 的 Agent 重启后，会先清理遗留容器；清理确认和 cleanup 回执完成后，任务才会在剩余重试预算内从头重新执行并记录恢复次数，预算耗尽时任务会失败。节点永久离线且无法确认容器已经停止时，活动任务会保持“待清理”并占用原节点，不会自动转移到其他节点；这是为了避免同一任务容器并行执行。显式重试的语义仍是 at-least-once，可能重复执行；非幂等任务应保持默认“不重试”。

同一个 Docker daemon（通常就是同一台物理节点）必须只运行一个 GPUFlow Agent。Agent 接管新 session 时会清理该 daemon 上所有带 `gpuflow.job` 标签的遗留任务容器，并在清理确认前保持节点不可调度；在同一 daemon 上运行多个 Agent 会破坏这一隔离与接管前提。

控制面与 Agent 必须使用同一版本镜像；升级包含 Agent 协议变更时，应先停止旧 Agent，再升级控制面和 Agent。旧 Agent 不会被兼容放行，以避免旧会话覆盖新任务 attempt。

## 提交任务

最简单的方式是在 Web 控制台的 **任务** 页面创建任务。仓库也提供了一个 CPU 示例：[examples/job.json](examples/job.json)。

获取或构建上述完整 GPUFlow 程序后，也可以通过同一个可执行文件提交任务：

```bash
./bin/gpuflow submit \
  -server http://localhost:18080 \
  -token "与 .env 中 GPUFLOW_TOKEN 相同的值" \
  -file examples/job.json
```

常用命令：

```text
gpuflow server [-addr :8080] -mysql-dsn DSN
gpuflow agent  [-server URL] [-pool default] [-executor docker|mock]
gpuflow submit -file examples/job.json
gpuflow jobs
gpuflow nodes
gpuflow get JOB_ID
```

`-mysql-dsn` 也可以通过必填环境变量 `GPUFLOW_MYSQL_DSN` 提供。启动控制面时还必须配置 `GPUFLOW_S3_ENDPOINT`、`GPUFLOW_S3_ACCESS_KEY` 和 `GPUFLOW_S3_SECRET_KEY`；完整示例见“一体化部署”和“从源码开发”。

CLI 命令也支持通过环境变量设置控制面地址和 Token：

```env
GPUFLOW_SERVER=http://localhost:18080
GPUFLOW_TOKEN=replace-with-a-long-random-token
```

任务队列支持按名称、ID、镜像或节点搜索，并可按状态、节点和资源池过滤。点击“重跑”会复制原任务配置并生成新的任务 ID；停止运行任务时，Agent 会执行 `docker stop` 并将状态更新为“已取消”。为避免丢失执行现场，只有已完成、失败或取消的任务可以删除。删除会先持久化为“删除中”，再清理对象存储产物并删除任务记录；任一步失败都可对同一任务重试删除，不会出现产物已丢失但任务恢复为普通终态的情况。

## 从脚本构建任务镜像

Web 控制台的 **镜像** 页面可以上传一个 `.py` 或 `.sh` 文件，并选择 Shell、Python 3.12、CUDA 12 或 PyTorch CUDA 基础环境。构建完成后，可以直接在任务表单中选择生成的镜像。镜像和节点列表都支持服务端搜索与分页。

删除任务镜像会同时删除控制端本地 Docker 镜像和 MySQL 中的 `task_images` 记录。构建中的镜像不能删除；仍被排队、已分配或运行中任务引用的镜像也不能删除。已完成任务只保留原镜像名称，不阻止镜像清理。

### 快速验证日志与产物下载

仓库中的 [examples/quick-smoke.sh](examples/quick-smoke.sh) 不需要额外依赖，可用于跑通完整流程：

1. 在 **任务镜像 → 上传任务脚本** 中选择该文件。
2. 运行环境选择 **Shell**，镜像名称填写 `gpuflow-task/quick-smoke:v1`，依赖留空。
3. 构建成功后点击 **使用此镜像提交任务**。CPU 节点将 GPU 数量和最低显存设为 `0`；真实 GPU 节点按实际资源填写。
4. 任务完成后在 **任务队列** 点击任务，可查看执行信息与日志，并下载包含 `result.txt` 的 `artifacts.tar.gz`。

GPU 检测示例 [examples/gpu-smoke.py](examples/gpu-smoke.py) 应选择 **PyTorch CUDA** 环境；PyTorch 已包含在基础镜像中，测试时依赖同样留空。

当前限制：

- 单个脚本最大 5 MB。
- 控制面所在主机必须能够访问 Docker。
- 构建过程会异步执行，并在页面中展示日志。
- 本地构建的镜像只存在于控制面主机。多节点部署时，应将镜像推送到所有 Agent 可访问的共享镜像仓库。
- 只应构建和运行可信代码。

## 配置

### 控制面

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `GPUFLOW_ADDR` | `:8080` | HTTP 监听地址 |
| `GPUFLOW_TOKEN` | 空 | API Bearer Token；对外部署时必须设置 |
| `GPUFLOW_PUBLIC_URL` | 当前页面来源 | Agent 命令使用的控制面公开地址 |
| `GPUFLOW_AGENT_IMAGE` | `gpuflow:local` | 接入页面生成 Docker Agent 命令时使用的镜像名；远程节点应设为明确版本或 Digest |
| `GPUFLOW_MYSQL_DSN` | 无 | 必填；任务、节点和任务镜像记录使用的 MySQL DSN |
| `GPUFLOW_S3_ENDPOINT` | 无 | 必填；MinIO/S3 兼容对象存储地址 |
| `GPUFLOW_S3_ACCESS_KEY` | 无 | 必填；对象存储 Access Key |
| `GPUFLOW_S3_SECRET_KEY` | 无 | 必填；对象存储 Secret Key |
| `GPUFLOW_S3_BUCKET` | `gpuflow-artifacts` | 任务产物存储桶 |
| `GPUFLOW_S3_REGION` | 空 | 可选；对象存储区域，MinIO 通常可留空 |
| `GPUFLOW_S3_USE_SSL` | `false` | 是否使用 HTTPS 连接对象存储 |

可复制 [.env.example](.env.example) 后按部署环境修改。

默认 Compose 已包含 MySQL 与 MinIO：

```bash
docker compose up --build -d
```

任务、节点和任务镜像记录分别写入 `jobs`、`nodes`、`task_images` 表，MySQL 是唯一核心状态真源。任务产物文件由 MinIO/S3 保存，不写入 MySQL。

### Agent

| 环境变量 | 说明 |
| --- | --- |
| `GPUFLOW_SERVER` | 控制面地址 |
| `GPUFLOW_TOKEN` | 与控制面一致的 Token |
| `GPUFLOW_NODE_ID` | 稳定且唯一的节点标识 |
| `GPUFLOW_NODE_NAME` | 节点显示名称 |
| `GPUFLOW_PROVIDER` | 可选高级配置；默认 `local` |
| `GPUFLOW_POOL` | 可选业务配置；默认 `default` |
| `GPUFLOW_GPU_PROBE` | 可选排障覆盖；默认自动探测 |
| `GPUFLOW_PROBE_IMAGE` | 可选排障覆盖；正常由控制面下发 |
| `GPUFLOW_CPU_CORES` | 可选；节点逻辑 CPU 核数，默认自动读取 Agent 进程可见的 CPU 数量 |
| `GPUFLOW_HOURLY_PRICE` | 可选业务配置；默认 `0` |
| `GPUFLOW_EXECUTOR` | 可选开发配置；生产默认 `docker` |
| `GPUFLOW_ARTIFACT_WORKDIR` | 可选；容器化 Agent 必须设置为宿主机与 Agent 容器共享的同路径目录 |

命令行参数会覆盖对应的环境变量。Agent 启动时自动识别 GPU 型号、数量和单卡显存汇总信息，并自动上报逻辑 CPU 核数；仅当容器 CPU 限额或宿主机探测结果不符合交付口径时，才使用 `-cpu-cores` 或 `GPUFLOW_CPU_CORES` 显式覆盖。

## 自动构建与发布

仓库内置 GitHub Actions 工作流，不需要在开发机手工制作发布产物：

- Pull Request 会运行 Go 测试、构建 Web，并验证 Docker 镜像能够构建，但不会发布。
- 推送到 `main` 会发布 `linux/amd64`、`linux/arm64` 的 `sha-<commit>` 镜像。
- 更新 `stable` Git 标签会发布 `stable` 镜像，并创建或更新唯一的 Community Stable GitHub Release。
- Stable Release 提供 Linux amd64、Linux arm64、Windows amd64 程序包和 `checksums.txt`。
- Stable Release 还会生成 `gpuflow-deployment-stable.tar.gz`，其中包含面向客户运维的独立 `README.md`、Compose、升级/回滚脚本和 Agent 部署模板。
- 发布镜像会按不可变 Digest 进行 Cosign 签名；Release 中的程序包和 `checksums.txt` 会附带 `.sigstore.json` 签名 bundle，并提供 `cosign.pub` 公钥。
- Community 只维护 `stable` 发布标识；日常 main 构建保留 `sha-*` 镜像用于定位和审计。

推送完成后可以直接拉取：

```bash
docker pull ghcr.io/stevenzhou789-cyber/gpuflow:stable
```

工作流使用仓库自动提供的 `GITHUB_TOKEN` 写入 GHCR 和 GitHub Release，不需要额外创建个人访问令牌。生产部署建议记录镜像 Digest，获得比版本标签更严格的产物固定。首次发布后需要在 GitHub Packages 中确认镜像包的公开访问设置。

下载 Release 中的 `cosign.pub` 和对应 bundle 后，可以验证镜像或安装包：

```bash
cosign verify --key cosign.pub ghcr.io/stevenzhou789-cyber/gpuflow@sha256:<digest>
cosign verify-blob --key cosign.pub \
  --bundle gpuflow-linux-amd64.tar.gz.sigstore.json \
  gpuflow-linux-amd64.tar.gz
```

## 从源码开发

环境要求：

- Go 1.25+
- Node.js 22+
- npm

构建 Web 前端、运行测试并生成完整 GPUFlow 程序：

```bash
cd web
npm ci
npm run build
cd ..
go test ./...
go build -trimpath -o bin/gpuflow ./cmd/gpuflow
```

Windows PowerShell 使用 `.exe` 文件名：

```powershell
npm --prefix web ci
npm --prefix web run build
go test ./...
go build -trimpath -o .\bin\gpuflow.exe .\cmd\gpuflow
```

启动控制面：

```bash
GPUFLOW_MYSQL_DSN='gpuflow:密码@tcp(127.0.0.1:3306)/gpuflow?parseTime=true&charset=utf8mb4' \
GPUFLOW_S3_ENDPOINT='127.0.0.1:9000' \
GPUFLOW_S3_ACCESS_KEY='gpuflow' \
GPUFLOW_S3_SECRET_KEY='替换为实际密码' \
./bin/gpuflow server -addr :8080
```

## 安全与部署边界

GPUFlow Agent 通过 Docker Socket 启动任务容器，这等同于拥有宿主机上的高权限。当前版本应部署在可信网络，并仅允许可信用户和可信镜像访问。

对外部署时至少应做到：

- 设置足够长且随机的 `GPUFLOW_TOKEN`。
- 使用 HTTPS 或放在可信反向代理之后。
- 限制控制面、Agent 和 Docker API 的网络访问范围。
- 不要在公共日志、截图或仓库中提交真实 Token。
- 对任务镜像设置来源限制，并为节点配置最小网络权限。

当前单控制面和共享 Token 设计不适合作为不受信任用户共同使用的公网多租户平台。

## 从开源验证到企业落地

GPUFlow Community 不是只能观看的演示版：它完整覆盖节点接入、批任务调度、实时日志、失败重试、产物归档以及 MySQL/MinIO 持久化，适合个人、实验室和小型团队在可信网络中真正跑任务。

> **核心区别：Community 按整台节点调度，Enterprise 按单张 GPU 调度。** Community 中一个任务占用节点后，该节点不会再接收其他任务；Enterprise 可把任务分配到明确的物理 GPU 索引，并通过 `CUDA_VISIBLE_DEVICES` 隔离可见设备，因此一台多卡服务器可以安全地并发运行多个任务。

Enterprise 中申请 `0` 张 GPU 的 CPU-only 任务仍按整台节点独占，既不会与另一个 CPU-only 任务并发，也不会与 GPU 任务混跑。这样可在 CPU 核数尚未纳入细粒度分配前避免节点过量承诺。

当 GPU 从“有人能用”走向“多人高效、安全地共用”，瓶颈通常不再是提交任务，而是资源利用率、镜像交付和团队治理。GPUFlow Enterprise 在同一套任务闭环之上补齐这些能力，无需迁移到另一套调度系统：

| 对比维度 | Community 社区版 | Enterprise 企业版 |
| --- | --- | --- |
| **调度粒度** | **按节点调度；一个任务运行时独占整台节点** | **按 GPU 调度；任务绑定具体物理 GPU 索引** |
| GPU 资源识别 | 启动时识别型号、数量和单卡显存汇总 | 识别 GPU UUID、索引、型号、显存、驱动和 Docker 环境 |
| 节点健康治理 | 启动阶段基础校验，不做周期健康状态治理 | 周期复检；异常节点标记 `DEGRADED`，持久化原因并停止接收新任务 |
| 多卡利用率 | 多卡节点同一时间只运行一个任务 | 按任务申请的 GPU 数分配，同一节点可并发运行多个任务 |
| 设备隔离 | 不区分节点内部的 GPU 索引 | 通过 `CUDA_VISIBLE_DEVICES` 向容器暴露已分配的 GPU |
| 任务镜像 | 对接外部共享 Registry，由管理员配置凭据 | 内置轻量 OCI Registry，自动完成构建、推送、凭据下发与节点拉取 |
| 任务可观测性 | 实时查看运行日志和任务产物 | 继承实时日志与产物能力，适合纳入企业交付和运维流程 |
| 访问控制 | 共享 Bearer Token，适合可信团队 | Admin、Operator、Viewer、Agent 分角色授权 |
| 审计与交付 | MIT 开源、自助部署 | JSONL 操作审计、离线 Ed25519 License、私有化交付与商业支持 |

Enterprise Agent 会按能力开关周期复检 Docker、NVIDIA Runtime 和逐卡清单。复检失败时节点进入 `DEGRADED` 并持久化原因，停止接收新任务；复检恢复后自动重新参与调度。License 到期不会中断正在执行的任务，已有节点仍可按原容量重连，但控制面会拒绝新增容量和新任务调度；降配或服务重启后，只有当前授权节点数与 GPU 数范围内的节点继续接收新任务。

如果你已经遇到以下任一情况，Enterprise 通常能直接带来价值：多卡节点只能同时跑一个任务；每台节点都要手工配置镜像仓库；共享 Token 无法区分人员权限；客户要求私有部署、操作留痕或交付支持。

> **先用 Community 验证任务，再让 Enterprise 接住规模化。** [提交 Enterprise 试用、私有化部署或合作需求](https://github.com/stevenzhou789-cyber/gpuflow/issues/new)，Issue 标题可注明 `[Enterprise]`，便于优先跟进。

## 当前边界

- 已支持本地 Docker 节点。
- 阿里云、腾讯云等托管适配器尚未实现。
- 当前调度面向批任务，不是 Kubernetes 替代品。
- 尚未提供跨控制面高可用和多租户隔离。

## 参与贡献

欢迎提交 Issue 和 Pull Request。报告问题时，请尽量附上：

- 操作系统、Docker、Go 和 Node.js 版本
- 控制面与 Agent 的启动方式
- 可复现步骤
- 预期结果与实际结果
- 已去除 Token 等敏感信息的日志

## License

GPUFlow 使用 [MIT License](LICENSE)。
