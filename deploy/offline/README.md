# GPUFlow 完整双架构离线部署

本目录是原 GitHub 交付契约的增量离线支持：原 `compose.yaml`、配置模板、
`README.md`、`PROJECT-README.md`、`LICENSE`、`VERSION`、全部 `scripts/` 和
`deploy/` 均保留；原安装说明、升级、程序回滚、SSH 批量 Agent 升级不删除。
GitLab 构建 Linux amd64 / arm64 镜像，以及 Linux amd64 / arm64 和 Windows
amd64 原生程序。每个架构离线包含该架构的控制面/Agent、Probe、MySQL、MinIO
四镜像，及三平台程序；社区版自身没有企业版任务 Registry/Nginx 依赖。

程序及部署 tar/zip、`checksums.txt` 均附原 Cosign `.sigstore.json` bundle。
离线包内 `SHA256SUMS` 也有 bundle；镜像索引在 GitLab Registry 以 Digest 签名。
使用已有签名信任链，不生成新私钥，不跳过缺失签名，不用 unsigned 精简包代替。
默认分支快照版本是 `v0.0.0-git.<commit>`，不是擅自宣布新的稳定发行版本。

## 前置条件与首次安装

目标主机须已安装 Linux、Bash、Docker Engine、Docker Compose v2、Cosign
v3.1.3、tar/gzip/sha256sum；选择与 Docker 服务端匹配的 amd64 或 arm64 包。
GPU 节点还需事先安装厂商驱动和容器运行时；这些宿主组件不属于应用镜像包。
请从独立可信渠道核对 Cosign 公钥，不能仅因为公钥随压缩包一起提供就信任它。

先在压缩包所在目录验证 bundle 与 checksum，再解压：

```bash
cosign verify-blob --offline --trusted-root trusted_root.json --key /trusted/cosign.pub \
  --bundle checksums.txt.sigstore.json checksums.txt
sha256sum --check checksums.txt
tar -xzf gpuflow-offline-VERSION-linux-amd64.tar.gz
cd gpuflow-offline-VERSION-linux-amd64
bash scripts/install-offline.sh --install-dir /opt/gpuflow \
  --public-key /trusted/cosign.pub --public-url http://CONTROL_PLANE_IP:18080
```

安装目标必须不存在，已有 `.env`、Compose 项目、数据卷不会被覆写。脚本生成
随机 Token/密码并保存为权限受限的 `.env`，不打印凭据；签名/文件校验后
`docker load` 并逐项验证镜像配置摘要（config digest）/架构，再以
`--pull never --no-build` 启动。这里不把 Docker 29 可能显示的索引/清单 ID
误当成旧版 Docker 的配置 ID；校验从本机重新导出的镜像内容计算配置摘要，
临时目录须有容纳一份未压缩单镜像归档的空间，不需要额外安装 Python/jq。
验收执行 MySQL `SELECT 1`、MinIO ready、控制面 healthz 和认证 nodes API。
社区版当前没有企业版 `/readyz`；此验收不等同于真实 GPU 任务验证。

GitLab 流水线还在空镜像库、全新数据卷、禁网的独立 Docker 29.3.1 守护进程
中实装 amd64 包，检查三服务运行和未带 Token 的 nodes 请求返回 401。
两架构包均须通过签名和镜像 manifest 检查；ARM 包校验不代表 ARM 真机实测。
只有该实际验收关卡通过的流水线，才能宣称完成此项离线实装验证。

首次启动之后可按原部署说明维护；本机离线启动继续显式传递
`--pull never --no-build`。不要执行在线 `compose pull`，不要 `down -v`。
若首次启动失败，数据与配置保留供排查；不要重新运行安装器覆盖现场。

## 升级、回滚与 Agent

原 `scripts/upgrade.sh`、`rollback.sh`、`upgrade-agents.sh` 保留原接口与备份、
失败恢复、运行任务保护。离线操作需先验证目标版本包、在每个目标节点导入
对应架构镜像，再使用这些脚本新增的 `--offline` 模式及明确的离线镜像仓库。
该模式不执行 `docker pull`，要求本地镜像匹配经过签名验证的 manifest。
不得把不兼容的旧程序直接用于当前数据库/产物格式，详见原 `README.md`。

Windows 原生 Agent 保留原方式，程序在 `agents/windows-amd64/gpuflow.exe`；
Linux 两架构在相应 `agents/` 子目录。离线 Docker Agent 节点也需导入本机架构
的 app/probe 镜像，设置控制面地址、Token、唯一节点 ID 后按原 Agent 模板启动。
两架构包使用相同的本地 Agent/Probe 镜像名称，每个节点只导入匹配本机架构的
包。因此控制面下发相同引用也可用于两种节点；配置摘要与架构仍分别强制
校验，不能把 amd64 镜像当作 arm64。SSH 离线升级按架构分别准备节点清单，
每次传入相应架构的 `--offline-package`；原在线升级方式仍保持兼容。

包已包含核心服务的全部镜像，不需联系 GitHub/GitLab/公网即可启动。
任务自己的镜像、Dockerfile 的基础镜像、模型、数据集及依赖需另外提前准备，
不能据“核心服务离线启动成功”声称任意训练/推理任务也已具备所有资源。
