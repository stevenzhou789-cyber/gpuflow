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

## 首次部署控制面

环境要求：Linux、Bash、Docker 和 Docker Compose v2。

```bash
cp .env.example .env
chmod +x scripts/*.sh
```

编辑 `.env`，至少替换 Token、MySQL 密码、MinIO 密码和公开访问地址。正式环境还应把下面两个变量设置为当前交付版本对应的镜像，例如：

```env
GPUFLOW_IMAGE=ghcr.io/stevenzhou789-cyber/gpuflow:v1.0.1
GPUFLOW_AGENT_IMAGE=ghcr.io/stevenzhou789-cyber/gpuflow:v1.0.1
```

启动并检查服务：

```bash
docker compose pull
docker compose up -d
docker compose ps
curl --fail http://127.0.0.1:18080/healthz
```

## 一键升级

升级前确认目标 `v*` 镜像已经发布，并且控制面磁盘有足够空间保存 MySQL 备份。然后在现有安装目录执行新交付包中的脚本：

```bash
./scripts/upgrade.sh v1.0.1
```

如果脚本位于新交付包、实际安装目录位于其他位置：

```bash
/path/to/new-package/scripts/upgrade.sh v1.0.1 \
  --install-dir /opt/gpuflow
```

脚本会依次完成：

1. 验证并拉取目标镜像。
2. 备份 `.env`、`compose.yaml` 和 MySQL。
3. 更新控制面和控制台生成的 Agent 镜像版本。
4. 重建控制面并检查 `/healthz`。
5. 配置了节点清单时，通过 SSH 逐台升级 Agent。

MySQL 和 MinIO 使用持久化 Volume，升级不会删除其中的数据。备份保存在安装目录的 `.gpuflow/backups/`。不要执行 `docker compose down -v`。

私有镜像仓库使用：

```bash
./scripts/upgrade.sh v1.0.1 \
  --image-repository harbor.example.com/gpuflow/gpuflow
```

只升级控制面：

```bash
./scripts/upgrade.sh v1.0.1 --skip-agents
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

编辑 `.env` 中的 Agent 镜像、控制面地址、Token 和唯一节点 ID。Agent 会自动采用控制面下发的 Probe 镜像，不需要在节点重复配置镜像地址；在线节点需能拉取该镜像，离线节点需提前导入交付镜像。Agent 会自动识别 GPU 型号、数量和显存汇总信息，并自动上报进程可见的逻辑 CPU 核数；仅在容器 CPU 限额或探测结果不准确时设置 `GPUFLOW_CPU_CORES`：

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
