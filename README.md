# GPUFlow Community

轻量级 BYOC（Bring Your Own Compute）GPU 批任务调度器。

GPUFlow 把分散在本地工作站、实验室服务器和自建机房中的 GPU 节点接入同一个控制面，统一完成任务提交、资源匹配、执行监控、失败重试与日志查看。算力仍由使用者自己提供，GPUFlow 不转售算力，也不介入云厂商计费。

Community 是面向个人、实验室和可信小团队的开源基础版，提供节点接入、批任务调度、日志、重试、产物和持久化。社区主线以小步维护、可靠性修复和共享内核兼容为主；完整的团队治理、运营报表与商业交付由私有仓库中的 Enterprise 提供。

共享内核允许公开和二次开发。官方社区发行版的功能范围不因底层存在共享模型或算法而自动扩大；具体 API、页面和构建边界见 [社区与企业版本边界](docs/EDITION-BOUNDARY.md)。

> 本页说明当前 `main` 源码。社区编号版本使用独立的 `vX.Y.Z` 标签，镜像和安装包以对应 Community Release 为准，可能落后于主线；企业版的同名标签不代表相同产品或配套产物。控制面与 Agent 应使用同一发行版、同一源码提交的配套产物。

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

## 社区版能力

- FIFO 任务队列、整节点独占调度；同一节点同时只执行一个任务
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

社区版展示节点的 GPU 型号、数量和显存汇总，不提供逐卡清单、驱动/Docker 版本详情或现成的企业管理页面。项目管理、角色权限、项目配额、公平调度、调度解释、节点维护、使用报表及镜像自动分发属于 [Enterprise 产品能力](#从开源验证到企业落地)。

## 快速开始

已有 Docker 环境且无需修改源码时，可从 [Community Releases](https://github.com/stevenzhou789-cyber/gpuflow/releases) 选择编号版本，下载部署包、`checksums.txt`、对应签名 bundle 和 `cosign.pub`，按包内 README 验签并部署。编号部署包已将控制面和 Agent 固定到同一镜像 Digest。标准部署包需要联网拉取系统及依赖镜像，不包含离线镜像归档。以下步骤适用于源码体验。

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

任务脚本只需把需要保留的文件写入 `$GPUFLOW_ARTIFACT_DIR`。Agent 在每次执行独有的目录归档结果并上传完整日志和 `artifacts.tar.gz`。上传中断会自动重试；打包或最终交付失败时任务明确报错，保留本地结果供回收，不自动重跑已经完成的计算。只有上传和最终状态均确认后才清理目录。重试窗口、大文件限制和恢复步骤见 [任务结果可靠性](docs/RESULT-RELIABILITY.md)。

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

升级前确认目标社区编号版本已成功发布、验签，并核对数据兼容性。社区 `vX.Y.Z`、历史 `stable` 和主线 `sha-*` 镜像各自独立；升级脚本接受编号版本，不能直接传入 `stable` 或 `sha-*`。记录当前镜像和目标镜像的 Digest，并使用目标社区部署包中的版本号：

```bash
# 在已下载并核验的目标社区部署包目录执行
TARGET_VERSION="$(cat VERSION)"
./scripts/upgrade.sh "$TARGET_VERSION"
```

脚本会先确认目标镜像存在，把 `.env`、`compose.yaml` 和 MySQL 备份到 `.gpuflow/backups/`，然后替换控制面容器并检查 `/healthz`。MySQL 与 MinIO Volume 不会被删除。健康检查失败时会自动切回原控制面镜像，但不会自动覆盖数据库。

默认镜像仓库为 `ghcr.io/stevenzhou789-cyber/gpuflow`。将社区镜像同步到自有私有仓库后可以指定：

```bash
./scripts/upgrade.sh "$TARGET_VERSION" \
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
./scripts/upgrade.sh "$TARGET_VERSION" --skip-agents
./scripts/upgrade-agents.sh "$TARGET_VERSION"
```

程序版本回滚同样从控制面执行：

```bash
ROLLBACK_VERSION='v<升级前已记录的社区版本号>'
./scripts/rollback.sh "$ROLLBACK_VERSION"
```

回滚脚本只切换控制面和 Agent 镜像，不恢复 MySQL 或 MinIO 数据。跨越不兼容数据库迁移之前，应先根据对应版本的升级说明判断旧程序是否能够读取当前数据库；需要恢复数据库备份时必须单独安排维护窗口，并接受备份时间点之后的数据会丢失。

产物发布格式也属于回滚兼容范围：当前控制面将每次上传保存在任务目录下独立的 `.gpuflow-versions/` 对象路径，并在 MySQL `jobs.requirements_json.artifact_refs` 中记录对外可见的文件引用。新控制面仍能读取旧版直接保存的产物，无需搬迁历史对象。旧控制面不认识这些引用，不能正确读取新版上传的产物；它写回任务时还可能丢弃引用。因此不要仅切换到不支持该格式的旧镜像并继续使用当前数据库。升级前应备份配套的 MySQL 和 MinIO/S3 数据；需要回退旧版本时，在维护窗口恢复与旧版本匹配的配套备份，或继续使用支持该格式的控制面。`rollback.sh` 不会自动完成数据恢复。

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

Windows x64 节点可以从与控制面版本一致的 [Community Release](https://github.com/stevenzhou789-cyber/gpuflow/releases) 下载 `gpuflow-windows-amd64.zip`，解压后得到 `gpuflow.exe`。若控制面使用当前主线或自定义构建，应从与控制面相同的源码提交构建 Agent：

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
| 可访问互联网的 Linux 节点 | 从 GHCR 拉取明确的版本镜像 | `ghcr.io/stevenzhou789-cyber/gpuflow:v1.0.0` |
| 多节点或企业内网 | 将同一版本的 GHCR 镜像同步到所有节点可访问的 Harbor 或其他私有仓库 | `harbor.example.com/gpuflow/gpuflow:v1.0.0` |
| 无法访问外网的离线节点 | 在联网机器拉取对应架构镜像，使用 `docker save` 导出后传入离线环境并执行 `docker load` | 导入后的本地镜像标签 |
| Windows 原生 Agent | 使用与控制面配套的 Community 对应版本程序包，或从同一源码提交构建 | `gpuflow.exe`，不需要 Agent 镜像 |
| 修改源码后的自定义部署 | 在仓库根目录执行 `docker build` | 自定义镜像标签 |

可联网的 Linux/Docker 环境直接拉取：

```bash
docker pull ghcr.io/stevenzhou789-cyber/gpuflow:v1.0.0
```

同步到私有仓库：

```bash
docker pull ghcr.io/stevenzhou789-cyber/gpuflow:v1.0.0
docker tag ghcr.io/stevenzhou789-cyber/gpuflow:v1.0.0 \
  harbor.example.com/gpuflow/gpuflow:v1.0.0
docker push harbor.example.com/gpuflow/gpuflow:v1.0.0
```

同步完成后，在控制端 `.env` 中设置镜像地址，Web 控制台生成的 Docker Agent 命令就会使用该地址：

```env
GPUFLOW_AGENT_IMAGE=harbor.example.com/gpuflow/gpuflow:v1.0.0
```

离线环境应明确选择目标 CPU 架构。以下示例导出 Linux amd64 镜像：

```bash
# 在可联网机器执行
docker pull --platform linux/amd64 ghcr.io/stevenzhou789-cyber/gpuflow:v1.0.0
docker save -o gpuflow-v1.0.0-linux-amd64.tar \
  ghcr.io/stevenzhou789-cyber/gpuflow:v1.0.0

# 将 tar 文件复制到离线节点后执行
docker load -i gpuflow-v1.0.0-linux-amd64.tar
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
  ghcr.io/stevenzhou789-cyber/gpuflow:v1.0.0 agent \
  -server "http://control-plane.example.com:8080" \
  -token "replace-with-your-token" \
  -id "lab-gpu-01" \
  -name "lab-gpu-01"
```

Agent 默认采用控制面下发的独立 glibc Probe 镜像：Windows/Linux 原生 Agent 先读取宿主机 `nvidia-smi`，容器 Agent 无法读取宿主机命令时会通过 Docker Socket 自动拉取并运行 Probe。社区版在启动时探测资源并上报节点汇总，默认不执行周期设备复检；GPU 环境变更或探测失败后，修复环境并重启 Agent 重新探测。周期复检、逐卡清单和自动健康恢复由企业版的节点健康治理提供。

正式节点应使用已核验的社区编号版本并记录 Digest，或直接固定 Digest；与控制端共用 Docker 的本地开发节点可以使用 Compose 自动构建的 `gpuflow:local`。产物工作目录必须以相同绝对路径挂载到 Agent 容器；Agent 直接作为宿主机进程运行时不需要设置该目录。实际接入时，建议优先复制控制台根据当前配置生成的完整命令。

Agent 应使用稳定且唯一的 `-id`。任务执行期间 Agent 会持续发送心跳。同一 ID 的 Agent 重启后，会先清理遗留容器；清理确认和 cleanup 回执完成后，任务才会在剩余重试预算内从头重新执行并记录恢复次数，预算耗尽时任务会失败。节点永久离线且无法确认容器已经停止时，活动任务会保持“待清理”并占用原节点，不会自动转移到其他节点；这是为了避免同一任务容器并行执行。显式重试的语义仍是 at-least-once，可能重复执行；非幂等任务应保持默认“不重试”。

同一个 Docker daemon（通常就是同一台物理节点）必须只运行一个 GPUFlow Agent。Agent 接管新 session 时会清理该 daemon 上所有带 `gpuflow.job` 标签的遗留任务容器，并在清理确认前保持节点不可调度；在同一 daemon 上运行多个 Agent 会破坏这一隔离与接管前提。

控制面与 Agent 必须使用同一版本镜像；升级包含 Agent 协议变更时，应先停止旧 Agent，再升级控制面和 Agent。旧 Agent 不会被兼容放行，以避免旧会话覆盖新任务 attempt。

## 提交任务

最简单的方式是在 Web 控制台的 **任务** 页面创建任务。仓库也提供了一个 CPU 示例：[examples/job.json](examples/job.json)。

创建任务时可指定 CPU 核数和主机内存 MiB，正值同时用于调度预留与容器运行限制。新建表单默认 1 核、2048 MiB；旧 API 省略或设为 0 时保持未限制行为。详细边界见 [CPU 与内存资源控制](docs/HOST-RESOURCES.md)。

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

产物文件的可见引用保存在任务的 `requirements_json.artifact_refs` 中，供控制面解析下载地址；引用不会通过任务 JSON 暴露。对象上传和复制不占用调度状态锁，只有通过当前 Agent 会话及任务尝试校验后才发布引用。被替换的版本保留至任务删除，删除任务时会一并清理版本对象；数据库提交结果不确定时也保留对象，避免重启后已提交的引用指向被删除的文件。

每次发布的引用由控制面记录所属执行次数（`attempt`）。产物列表、产物下载和完整日志下载只返回当前执行次数的文件；下一次重试开始分配后，即使没有生成文件或上传失败，也不会返回上一次执行的文件。历史对象不会因此删除，仍随任务删除统一清理。旧版未记录执行归属的文件在任务未重试时继续可读；已经重试的历史任务无法可靠判断文件属于哪次执行，因此升级后不再将这些文件作为当前结果提供，也不会猜测或补填归属。此兼容处理无需数据库表结构迁移。

回滚时控制面还必须支持引用中的 `attempt` 字段及下载筛选；仅支持旧 `artifact_refs` 格式的程序仍会重现跨执行混用，并可能在写回任务时丢弃执行归属。不要让这样的旧程序继续读写采用修复后的数据；需要回退时按前述配套备份恢复流程处理。

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

仓库内置 GitHub Actions 工作流；以下产物仅在对应测试、构建及签名步骤成功后可用：

- Pull Request 会运行 Go 测试、构建 Web，并验证 Docker 镜像能够构建，但不会发布。
- 推送到 `main` 会发布 `linux/amd64`、`linux/arm64` 的 `sha-<commit>` 镜像。
- 首次推送新的 `vX.Y.Z` 标签会触发社区编号版本发布：检查原始标签事件和提交、完成测试、构建及验签，再发布 GitHub Release 和同名镜像。编号版本拒绝替换已有发布资产或不同 Digest 的版本镜像。
- `stable` 标签事件会触发 `stable` 镜像和 Community Stable GitHub Release 构建；推送 `main` 不会自动更新它。标签操作按仓库授权流程执行。
- Community Release 提供 Linux amd64、Linux arm64、Windows amd64 程序包和 `checksums.txt`。
- 部署包命名为 `gpuflow-deployment-<标签>.tar.gz`，其中包含独立运维 `README.md`、Compose、升级/回滚脚本和 Agent 部署模板；生成时固定本次镜像 Digest，并检查交付文件边界。
- 发布镜像会按不可变 Digest 进行 Cosign 签名；Release 中的程序包、部署包和 `checksums.txt` 会附带 `.sigstore.json` 签名 bundle，并提供 `cosign.pub` 公钥。
- 编号发布中断后只重试原工作流的失败任务；已有草稿仅允许原运行补齐经核验的缺失文件。不得移动、删除或重建版本标签来重试。
- 仓库边界检查覆盖跟踪文件、构建源码和新生成的前端；私有产品代码、本地配置、密钥与运行数据不得进入社区产物。

选择社区版本时，确认对应工作流和 Release 成功后拉取，例如：

```bash
docker pull ghcr.io/stevenzhou789-cyber/gpuflow:v1.0.0
```

工作流使用仓库自动提供的 `GITHUB_TOKEN` 写入 GHCR 和 GitHub Release；签名还需要仓库已配置的 `COSIGN_PRIVATE_KEY` 和 `COSIGN_PASSWORD`。生产部署应记录并核验镜像 Digest。首次发布后需要在 GitHub Packages 中确认镜像包的公开访问设置。

GitHub 与 GitLab 各自保留构建检查，推送成功不等于构建或发布成功。GitLab 当前仍拒绝 tag 发布；其社区 SHA 开发构建即使带签名，也不等于 GitHub 已发布的 Community 编号安装包。

下载 Release 中的 `cosign.pub` 和对应 bundle 后，可以验证镜像或安装包：

```bash
cosign verify --key cosign.pub ghcr.io/stevenzhou789-cyber/gpuflow@sha256:<digest>
cosign verify-blob --key cosign.pub \
  --bundle gpuflow-linux-amd64.tar.gz.sigstore.json \
  gpuflow-linux-amd64.tar.gz
```

## 从源码开发

共享内核内部使用自动创建的 `default` 项目，已有任务归入该项目。官方社区版保持 FIFO 与整节点独占，不开放项目管理、项目 Token、公平调度或调度解释，也不记录企业调度决策。共享模型与算法可用于自行研发；启用公开能力字段不会提供私有仓库中的完整企业产品。

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

GPUFlow Community 提供完整的基础任务闭环，适合个人、实验室和小型团队在可信网络中运行任务。官方社区产品保留整节点独占、共享 Token 和自助运维的边界。

Enterprise 在私有仓库中提供逐卡并发调度，以及项目身份与配额、角色权限、节点维护、调度解释、历史使用报表、操作审计和镜像自动分发。企业产品的价值来自这些能力与受支持交付的完整组合；共享调度内核可以继续公开和复用。

Community 中一个任务占用节点后，该节点不会再接收其他任务。Enterprise 将 GPU 任务绑定到分配的物理设备，通过容器运行时限制可见设备，使一台多卡服务器能够并发运行多个任务。Enterprise 中申请 `0` 张 GPU 的 CPU-only 任务仍按整节点独占，不与其他任务混跑。

以下对比说明当前产品代码边界；实际企业交付范围以对应正式发布版本为准：

| 对比维度 | Community 社区版 | Enterprise 企业版 |
| --- | --- | --- |
| **调度粒度** | **FIFO、整节点独占；同一节点同时只执行一个任务** | **逐卡并发、按项目权重公平调度与项目内任务优先级** |
| GPU 资源识别 | 启动时识别型号、数量和显存汇总；不提供逐卡/驱动/Docker 明细 | GPU UUID、索引、型号、显存及驱动、Docker 环境明细 |
| 节点健康治理 | 启动探测、基础健康状态处理、心跳/离线与会话清理；默认关闭周期设备复检 | 周期复检、异常原因留存、停止新分配及复检恢复 |
| 多卡利用率 | 多卡节点同一时间只运行一个任务 | 按任务申请的 GPU 数分配，同一节点可并发运行多个任务 |
| 设备可见范围 | 不按任务分配节点内部的 GPU 索引 | 通过容器运行时向任务暴露已分配设备 |
| 项目治理 | 内部单一 `default` 项目，无项目管理与项目 Token 接口 | 项目管理、项目 Token、项目范围校验及队列、并发任务、GPU 配额 |
| 任务镜像 | 控制面本地构建；管理员负责向外部 Registry 分发及配置节点凭据 | 内置 OCI Registry，自动推送、凭据下发与节点拉取 |
| 调度观测与维护 | 任务状态、日志和产物；无调度解释和维护管理入口 | 排队原因、分配决策、项目公平状态；人工排空、停止新分配与恢复 |
| 使用量与运营 | 节点报价用于资源选择，无企业使用报表和独立历史账本 | 历史设备占用量、估算成本、项目/时间范围汇总及 CSV |
| 访问控制 | 共享 Bearer Token，适合可信团队 | Admin、Operator、Viewer、Agent 分角色授权 |
| 操作审计 | 无企业业务操作审计 | HTTP 业务操作审计与角色控制 |
| 交付方式 | MIT 开源、自助部署和社区维护 | 离线授权、签名离线部署包与商业交付 |

设备可见范围和项目访问控制不等于完整的多租户安全隔离。节点维护不表示自动滚动升级，使用量与成本参考也不等于财务结算。

Enterprise Agent 会按能力开关周期复检设备清单和对应的容器运行环境。复检失败时节点进入 `DEGRADED` 并持久化原因，停止接收新任务；复检恢复后自动重新参与调度。License 到期不会中断正在执行的任务，已有节点仍可按原容量重连，但控制面会拒绝新增容量和新任务调度；降配或服务重启后，只有当前授权节点数与 GPU 数范围内的节点继续接收新任务。

当团队需要共享多卡节点、按项目管理配额、区分人员权限、查看使用报表，或统一镜像分发与离线交付时，可以评估 Enterprise。

> **先用 Community 验证任务，再让 Enterprise 接住规模化。** [提交 Enterprise 试用、私有化部署或合作需求](https://github.com/stevenzhou789-cyber/gpuflow/issues/new)，Issue 标题可注明 `[Enterprise]`，便于优先跟进。

## 当前边界

- 已支持本地 Docker 节点。
- 阿里云、腾讯云等托管适配器尚未实现。
- 当前调度面向批任务，不是 Kubernetes 替代品。
- 尚未提供跨控制面高可用和多租户隔离。

## 参与贡献

社区主线优先接受缺陷修复、可靠性与文档改进、必要的共享内核兼容调整。新增官方产品能力需先明确版本边界。开发者仍可按 [MIT License](LICENSE) 自行修改和扩展公开代码；私有企业产品实现不包含在本仓库中。

欢迎提交 Issue 和 Pull Request。报告问题时，请尽量附上：

- 操作系统、Docker、Go 和 Node.js 版本
- 控制面与 Agent 的启动方式
- 可复现步骤
- 预期结果与实际结果
- 已去除 Token 等敏感信息的日志

## License

GPUFlow 使用 [MIT License](LICENSE)。
