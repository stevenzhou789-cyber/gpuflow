# GPUFlow 部署与升级说明

本文档面向 GPUFlow 控制面服务器管理员。所有升级和回滚命令均在控制面服务器执行；Linux/Docker Agent 可以通过 SSH 从控制面集中更新。

## 目录说明

```text
compose.yaml                 控制面、MySQL 和 MinIO
.env.example                控制面配置模板
scripts/upgrade.sh          一键升级入口
scripts/rollback.sh         程序版本回滚入口
scripts/upgrade-agents.sh   Agent 批量升级工具
scripts/agents.conf.example 节点 SSH 清单模板
deploy/agent/               Linux/Docker Agent Compose 模板
VERSION                     当前交付包版本
```

客户的真实 `.env`、`scripts/agents.conf`、SSH 私钥和数据库备份不属于交付包，请勿提交或传回公共仓库。

## 核验部署包

从同一个 Community Release 下载部署包、`checksums.txt`、它们各自的 `.sigstore.json` 和 `cosign.pub`。先通过已信任的渠道核对签名公钥，再使用 Cosign 核验；仅从同一下载位置获取公钥不能独立确认发布者身份。

```bash
cosign verify-blob --key cosign.pub --bundle checksums.txt.sigstore.json checksums.txt
# 将版本号替换为下载的 Community 版本
PACKAGE=gpuflow-deployment-v1.0.0.tar.gz
cosign verify-blob --key cosign.pub --bundle "$PACKAGE.sigstore.json" "$PACKAGE"
sha256sum --check --ignore-missing checksums.txt
tar -xzf "$PACKAGE"
cd "${PACKAGE%.tar.gz}"
```

Release 同时提供 Linux amd64、Linux arm64 和 Windows amd64 原生程序包，各有独立签名。标准部署包包含配置和运维脚本，需要联网拉取 GPUFlow、MySQL、MinIO 和 Probe 镜像，不包含离线镜像归档。社区编号版本与企业版分别发布，不可混用。

## 首次部署控制面

环境要求：Linux、Bash、Docker 和 Docker Compose v2。

```bash
cp .env.example .env
chmod +x scripts/*.sh
```

编辑 `.env`，至少替换 Token、MySQL 密码、MinIO 密码和公开访问地址。编号部署包的 `GPUFLOW_IMAGE`、`GPUFLOW_AGENT_IMAGE` 和 Agent 配置模板已固定为本次构建的同一镜像 Digest；保留这些值即可拉取配套镜像。包内 Compose 无需本地源码构建。首次运行前也应使用已信任公钥执行 `cosign verify --key /path/to/cosign.pub <GPUFLOW_IMAGE的完整值>` 核验镜像。

启动并检查服务：

```bash
docker compose pull
docker compose up -d
docker compose ps
curl --fail http://127.0.0.1:18080/healthz
```

## 一键升级

升级前确认目标社区 `vX.Y.Z` 镜像已经发布并完成验签，并且控制面磁盘有足够空间保存 MySQL 备份。升级脚本检查版本格式并拉取镜像，不会自动调用 Cosign。先在已核验的新交付包目录读取目标版本，再指定现有安装目录：

```bash
TARGET_VERSION="$(cat VERSION)"
./scripts/upgrade.sh "$TARGET_VERSION" --install-dir /opt/gpuflow
```

如果脚本位于新交付包、实际安装目录位于其他位置：

```bash
/path/to/new-package/scripts/upgrade.sh "$TARGET_VERSION" \
  --install-dir /opt/gpuflow
```

脚本会依次完成：

1. 检查版本格式并拉取目标镜像。
2. 备份 `.env`、`compose.yaml` 和 MySQL。
3. 更新控制面和控制台生成的 Agent 镜像版本。
4. 重建控制面并检查 `/healthz`。
5. 配置了节点清单时，通过 SSH 逐台升级 Agent。

MySQL 和 MinIO 使用持久化 Volume，升级不会删除其中的数据。备份保存在安装目录的 `.gpuflow/backups/`。不要执行 `docker compose down -v`。

私有镜像仓库使用：

```bash
./scripts/upgrade.sh "$TARGET_VERSION" \
  --image-repository harbor.example.com/gpuflow/gpuflow
```

只升级控制面：

```bash
./scripts/upgrade.sh "$TARGET_VERSION" --skip-agents
```

## 配置 Agent 集中升级

现有 Agent 如果由临时 `docker run` 命令启动，需要先迁移一次到 Compose 管理。把交付包中的 `deploy/agent` 复制到每个节点，例如 `/opt/gpuflow-agent`：

```bash
sudo mkdir -p /opt/gpuflow-agent
sudo cp deploy/agent/compose.yaml deploy/agent/.env.example /opt/gpuflow-agent/
sudo chown -R "$USER":"$USER" /opt/gpuflow-agent
cd /opt/gpuflow-agent
mv .env.example .env
```

编辑 `.env` 中的控制面地址、Token 和唯一节点 ID，保留包内已固定的 Agent 镜像 Digest。Agent 会自动采用控制面下发的 Probe 镜像，不需要在节点重复配置镜像地址；在线节点需能拉取该镜像，离线节点需自行准备并提前导入对应架构镜像。Agent 会自动识别 GPU 型号、数量和显存汇总信息，并自动上报进程可见的逻辑 CPU 核数；仅在容器 CPU 限额或探测结果不准确时设置 `GPUFLOW_CPU_CORES`：

```bash
docker compose up -d
docker compose ps
```

在控制面服务器创建节点清单：

```bash
cp scripts/agents.conf.example scripts/agents.conf
```

每行格式为 `SSH目标|Agent绝对安装目录`：

```text
ops@gpu-01|/opt/gpuflow-agent
ops@gpu-02|/opt/gpuflow-agent
```

控制面必须能够通过 SSH 公钥登录节点，登录用户必须有 Docker 权限以及 Agent 目录写权限。SSH 端口、堡垒机和 `ProxyJump` 请配置在 `~/.ssh/config`，不要把密码或私钥写进清单。

节点上存在运行中的 GPUFlow 任务容器时，升级脚本会拒绝中断该节点并停止批量升级。任务完成后重新运行相同版本的升级命令即可。Windows 原生 Agent 不在此 SSH/Docker 自动升级范围内。

## 程序回滚

回滚控制面和 Linux/Docker Agent：

```bash
./scripts/rollback.sh v1.0.0
```

只回滚控制面：

```bash
./scripts/rollback.sh v1.0.0 --skip-agents
```

回滚默认只切换程序镜像，不恢复 MySQL 或 MinIO 数据。数据库结构发生不兼容变化时，不要直接运行旧程序；应先查阅对应版本说明。恢复 MySQL 备份会丢失备份时间点之后的数据，必须单独安排维护窗口。

产物发布格式同样需要版本匹配。当前版本使用任务目录下的 `.gpuflow-versions/` 对象和 MySQL `jobs.requirements_json.artifact_refs` 引用，仍兼容读取旧版直接保存的产物。旧控制面不支持这些引用，会看不到新版上传的文件，写回任务还可能丢弃引用。因此不能仅运行回滚脚本后让不支持该格式的旧程序继续读写当前数据。升级前备份配套的 MySQL 与 MinIO/S3；确需回退旧版时，在维护窗口恢复与目标版本匹配的配套备份，或使用支持当前产物格式的控制面。回滚脚本不会自动恢复备份。

产物引用还包含控制面记录的执行次数 `attempt`，产物列表、下载及完整日志只提供当前执行的文件。重试后没有上传成功的新文件时，不回退读取前一次文件。未重试的旧任务保持兼容；已重试且缺少执行归属的旧文件无法可靠识别，升级后隐藏其当前结果入口，保留存储对象至任务删除。该修复无需表结构迁移；回退到不识别 `attempt` 的控制面会重新引入结果混用，并可能丢失归属字段，须遵循配套备份恢复要求。

## 常见排查命令

```bash
docker compose ps
docker compose logs --tail 200 control-plane
docker compose logs --tail 200 mysql minio
cat .gpuflow/current-version
cat .gpuflow/last-backup
```

Agent 节点：

```bash
cd /opt/gpuflow-agent
docker compose ps
docker compose logs --tail 200 agent
docker ps --filter label=gpuflow.job
```
