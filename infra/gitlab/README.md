# GPUFlow 本地 GitLab 运维

本配置用于 Windows Docker Desktop（WSL2 / Linux 容器）的本机开发与交付验证，不是生产部署或团队高可用代码服务器。本地 GitLab 中的社区版、企业版、许可证工具均为私有项目。**社区版和企业版按当前用户要求同步到 GitHub 与 GitLab，保留两端 CI 并分别报告结果。** 其他项目遵循各自用户授权；任一服务的配额或构建失败都不等于两端代码同步失败，也不能据此放宽发布门禁。

本机停机、休眠、Docker Desktop 关闭或磁盘故障都会影响 GitLab。下文说明配置和验收要求；只有对应流水线、报告及下载后的产物验证全部成功，才能宣布本次构建完成。

## 地址与资源

| 服务 | 地址 | 宿主绑定 |
| --- | --- | --- |
| 浏览器登录 | `http://127.0.0.1:8088` | `127.0.0.1:8088` |
| HTTP Git 规范地址 | `http://gitlab.gpuflow.test:8088` | 同上 |
| Git SSH | `ssh://git@127.0.0.1:2224` | `127.0.0.1:2224` |
| Container Registry | `gitlab.gpuflow.test:5055` | `127.0.0.1:5055` |

三个端口仅绑定回环地址，不对局域网或公网开放。浏览器可直接用 IP 登录；无需管理员权限或修改 Windows hosts。页面生成的部分 clone / Registry 地址使用规范域名，按下面的仓库级解析配置处理，不要因此更改全局 DNS、代理或 hosts。

Docker 内网为 `gpuflow-devops` / `172.30.88.0/24`，GitLab 为 `.2`，builder 为 `.3`；如与 VPN/已有网络冲突，应先审查并一致调整 Compose 和 Runner 配置，不能只改一处。

容器上限：GitLab 3.5 GiB / 4 CPU，builder 2 GiB / 2 CPU，Linux Runner 256 MiB / 1 CPU。Linux Runner 串行执行一个作业，单作业上限 1.5 GiB / 2 CPU；Go 编译并发为 2，Node 堆上限 768 MiB。Windows Runner 另有一个串行槽，两个 Runner 可能同时运行；Windows shell 作业不受 Docker 内存上限保护。还需给 Windows、WSL2 和 Docker 留余量。这是低资源本地配置，不代表生产规格；OOM 时先检查资源，不能跳过架构、测试或安全门禁来换取成功状态。

## 首次启动

从包含本目录的社区版仓库根目录执行。先打开 Docker Desktop，确认运行 Linux 容器，且 `docker info` 成功：

```powershell
docker compose -f .\infra\gitlab\compose.yaml up -d gitlab
docker compose -f .\infra\gitlab\compose.yaml ps
```

首次初始化可能需要数分钟。GitLab healthy，或以下 readiness 成功后，执行初始化与 Linux Runner 注册：

```powershell
docker exec gpuflow-gitlab curl --fail --silent http://127.0.0.1:8088/-/readiness
powershell -NoProfile -ExecutionPolicy Bypass -File .\infra\gitlab\bootstrap.ps1
powershell -NoProfile -ExecutionPolicy Bypass -File .\infra\gitlab\register-runner.ps1
docker compose -f .\infra\gitlab\compose.yaml ps
```

`bootstrap.ps1` 关闭公开注册，校验三个私有项目，配置企业版读取固定社区提交的 Job Token allowlist，创建受限 Git 推送凭据与 group Runner。`register-runner.ps1` 启动 builder 并注册 `linux-docker` Runner。凭据目录默认位于该社区仓库的 `.local-gitlab`；如从隔离迁移副本调用而要复用原工作区的私有状态，先查看脚本参数，显式使用正确的状态目录，不能重新创建一套无关凭据。

项目路径为 `gpuflow/gpuflow`、`gpuflow/gpuflow-enterprise`、`gpuflow/gpuflow-license-issuer`。保持项目和产物访问私有、最新产物不无限期保留。只同步审查后的源码，不将未跟踪的环境文件、密钥、许可证或缓存加入 Git。

### 真实 Windows PowerShell 5.1 Runner

企业版原 Windows 兼容性关卡必须在真实 Windows PowerShell 5.1 下运行，Linux PowerShell 7 不能替代。用当前普通 Windows 用户启动：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\infra\gitlab\register-windows-runner.ps1 -LocalStateDir D:\workspace\gpuflow\.local-gitlab
```

替换为实际已初始化的状态目录。脚本校验固定版本 Runner 二进制 SHA256，注册仅接收受保护 ref 的 `windows-powershell51` group Runner，禁用 untagged 作业及 debug trace，限制并发为 1。它不安装系统服务、不创建自动登录任务、不修改 hosts、不请求系统提权，而是隐藏启动用户模式后台进程。私有目录中的 `process.json` 记录 PID、启动时间及日志路径；重复调用会核对现有进程，避免误接管或重复启动。

Windows 注销、重启或进程退出后，需要同一用户重新运行脚本；不要把“隐藏后台运行”当成开机自启。普通 Compose stop 不会停止此 Windows 进程，维护前还需暂停该 Runner 并等待作业结束。

Windows PowerShell 5.1 会删除被赋空值的环境变量，因此 Runner 的 `GIT_CONFIG_COUNT` 只能引用解析和长路径这两个非空配置项。代理绕过由本机 `NO_PROXY` 和清空进程代理变量完成，不修改全局 Git。旧 COUNT=5 配置应先暂停、排空并停止准确匹配的 Runner，再运行 `update-windows-runner-git-env.ps1 -LocalStateDir <私有状态目录> -ExpectedRunnerId 2 -ConfirmRunnerStopped`。该脚本只迁移对应环境项并保存私有回滚副本，不重新注册、改令牌或自行停止进程；随后用注册脚本启动原 Runner。`test-register-windows-runner.ps1` 可在真实 PS5.1 中运行无真实 API/凭据的回归。

注册令牌另存为 DPAPI 凭据。Runner 自用的 `config.toml` 必须含认证信息，因此整个 Windows Runner 状态、构建和日志目录都限制为当前用户与 SYSTEM，不能提交或上传为 artifact。Shell executor 使用该用户权限运行，**不是沙箱**；只能处理已审查的可信代码，不能接入任意 fork/MR 或外部贡献者。

## 登录与凭据

浏览器打开 `http://127.0.0.1:8088`，管理员用户名为 `root`。初始化脚本将登录凭据存入 `.local-gitlab/admin-login.credential.xml`，使用 Windows DPAPI 加密并限制目录 ACL；一般只有创建它的 Windows 用户在原机器上能解密。该目录必须始终被 Git 忽略。

如需查看密码，由你自己在本机、没有录屏或 PowerShell transcript 的终端执行：

```powershell
(Import-Clixml -LiteralPath .\.local-gitlab\admin-login.credential.xml).GetNetworkCredential().Password
```

这条命令会显示密码；不要把结果粘贴到聊天、工单、日志、文档或截图，不要在 CI 中执行。不要用 root 密码作为 CI 变量，也不要打印其他凭据文件。更换 Windows 用户或重装机器后不能假定仍能解密；管理员改密码后应自行更新安全保存方式，不能盲目重置已有账号。

初始化 API、Git 推送、Linux/Windows Runner 和签名凭据用途不同。推送令牌过期应受控轮换；不能统一改用长期管理员令牌。签名私钥及密码只能按受保护、受限制的签名作业配置，不能放入源码、镜像层、构建日志或交付包。

## CI 信任边界

Linux Runner 和作业不挂载宿主 `/var/run/docker.sock`。作业通过 `tcp://builder:2375` 使用独立 DinD daemon，API 不发布宿主端口。DinD 本身仍是 privileged 容器，不是强多租户安全沙箱。仅运行这三个可信私有项目的受保护代码。

Registry 当前为本机 HTTP，builder 只为 `gitlab.gpuflow.test:5055` 配置例外。签名操作对 HTTP 的例外也必须限定到此 Registry；不能关闭全局 TLS 验证或把该 Registry 暴露到公网。宿主 Docker Desktop 自行拉取时，需要为本机 Registry 配置相应解析及精确地址例外；不影响通过独立 builder 完成 CI 构建。

`Dockerfile.ci-tools` 定义完整 Linux 工具镜像，包括 Go、Node、Docker/Buildx/Compose、PowerShell 7、Playwright、Cosign 与 Trivy。镜像构建、工具完整性校验成功后，应将 CI 绑定到已验证的不可变 Digest；单独写入一个镜像 tag 不代表工具已经可用。真实 Windows 5.1 作业仍由上述 Windows Runner 执行。

## 完整 SHA 离线交付与验收

迁移的目标不是单架构、未签名的缩水包。按源码 SHA 构建完整交付，同时保留正式版本发布的边界：

| 项目 | 必须具备的离线交付内容 |
| --- | --- |
| 社区版 | Linux amd64、arm64 两套离线包，每套 4 个运行镜像：应用/Agent、Probe、MySQL、MinIO；Linux amd64、arm64 与 Windows amd64 程序包；完整校验和与签名材料 |
| 企业版 | Linux amd64、arm64 两套离线包，每套 7 个运行镜像：企业应用/Agent、Probe、MySQL、MinIO、Registry、Nginx、备份 Alpine；安装/升级/回滚材料、三平台 Agent、风险批准、SBOM、校验和与签名材料 |
| 许可证工具 | Linux amd64、arm64 与 Windows amd64 原生程序归档、校验和及构建来源记录；不制造无必要的容器，不执行真实签发、不包含私钥或签发记录 |

许可证工具内嵌界面，程序包可离线运行，无需另装 Go、Node、Docker 或连接 GitHub/GitLab。程序本身没有操作系统代码签名时，必须明确标为 unsigned；归档校验和的供应链签名不能冒充 Windows Authenticode。实际签发密钥由使用者运行时另外保管和选择。

企业版完整门禁包括原 Go/UI 和交付脚本验证、Linux PowerShell 7 与真实 Windows PowerShell 5.1 安装/升级回归、五个固定 Digest 依赖扫描、应用与 Probe 扫描、双架构七镜像 SBOM、Docker 导出/导入往返与平台/镜像 ID 一致性检查、现有公钥对应的签名及验签、包内容和校验和完整性验证。社区版同样不能遗漏原构建与签名契约及新增离线包的完整性验证。应以各自 CI 的实际关卡及证据逐项验收，不能把另一仓的成功作为本仓成功。

缺工具、缺架构、扫描出错、报告缺失、批准过期、签名不匹配、往返或包校验失败都必须失败，不能静默降级、忽略错误或只上传日志后声称交付完成。既有 `v1.1.3` 风险批准只覆盖文档中精确依赖 Digest 且有期限，不能为新版本、新 Digest 或新问题自动续批。扫描报告与完整交付证据按 CI 配置保留 90 天；许可证开发程序包按其 CI 配置保留 7 天。必须核对 artifact 上传成功且能下载，构建容器内生成文件并不等于交付完成。

SHA 构建即使镜像和校验和已签名，也不等于批准发布新的 `v*` 正式版本。社区版的正式 tag 发布事务尚未完整迁移，tag 流水线明确失败；企业版的正式发布由其独立的标签、审批、验证和发布证据门禁控制。不要移动、删除、重打或重放已有 `v1.1.3`。镜像有独立生命周期，应配置 SHA 镜像清理规则，不能删除正在部署或仍需回滚的 Digest。

## 日常双端同步工作流

配置单一推送 URL 的 GitHub `origin` 和本地 `gitlab` 两个远端。社区版和企业版均自带双端推送脚本，无需依赖兄弟仓库布局。提交前审查 diff、运行测试、显式选择源码文件；本地 commit 经授权完成后，在该仓库执行：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\push-all.ps1
```

也可显式指定实际仓库的 `-RepoPath`。迁移阶段使用隔离副本的真实路径，不能混入原始仓库。脚本不 stage/commit，不处理未跟踪文件，不创建 tag、不强推；它冻结当前提交 SHA、使用显式 refspec，并分别核验两端远端 SHA。tracked 文件未提交时会阻止推送；两端推送不是跨服务原子事务，必须明确报告部分失败。CI 状态也应按服务分别报告，推送成功不等于构建成功。

`push-all.ps1` 是默认双端入口；`push-gitlab.ps1` 保留为获得明确授权时的单端辅助工具。仅在明确要求推送一个已存在且获准的 tag 时才使用 `push-all.ps1 -Tag <已有tag>`，这不会绕过 CI 的正式发布门禁。

规范域名的 Git 访问使用以下仓库级配置（`.git/config`），不改全局 Git 或 hosts。迁移初始化应设置并核验它；不要在 PowerShell 5.1 中依赖原生命令的空字符串传参来设置空代理：

```ini
[http]
    curloptResolve = gitlab.gpuflow.test:8088:127.0.0.1
[http "http://gitlab.gpuflow.test:8088"]
    proxy =
```

这些设置只适用于本机 GitLab 的当前地址；Windows Runner 对应解析和空代理通过作业专属环境配置注入。Linux Runner 使用内部网络映射。浏览器直接访问 IP，无需上述 Git 配置。未来迁移到其他机器时，应受控更新各处地址，不能继续把远端解析到本机。

## 停止、重启、备份

先暂停新的 CI 作业，等待 Linux 和 Windows Runner 上正在执行的作业结束。日常暂停与恢复使用以下命令，它们保留命名卷：

```powershell
docker compose -f .\infra\gitlab\compose.yaml stop
docker compose -f .\infra\gitlab\compose.yaml up -d
```

这不管理独立的 Windows Runner 进程。不要执行 `docker compose down -v`、`docker volume prune`、`docker system prune --volumes` 或 Docker Desktop 清空/恢复出厂设置来“修复构建”，这些操作可能永久删除代码、数据库、镜像、产物和密钥。普通容器重建也不是备份。

需保护的命名卷：

- `gpuflow-gitlab-data`：数据库、仓库、应用数据、产物、Registry 数据。
- `gpuflow-gitlab-config`：GitLab 配置、`gitlab-secrets.json` 及相关密钥。
- `gpuflow-gitlab-logs`：服务日志，注意敏感信息与保留时间。
- `gpuflow-gitlab-runner-config`：Linux Runner 注册配置与认证信息。
- `gpuflow-gitlab-builder-data`：构建 daemon 镜像和缓存，清理前仍须核实目标。

在维护窗口按 [GitLab Docker 备份说明](https://docs.gitlab.com/install/docker/backup/) 生成应用备份：

```powershell
docker exec gpuflow-gitlab gitlab-backup create
```

备份默认位于容器 `/var/opt/gitlab/backups`。还需单独备份 `/etc/gitlab`，尤其 `gitlab-secrets.json`、本目录配置和 Linux Runner 配置；本机 `.local-gitlab` 与 Windows Runner 私有状态也需要受控备份，DPAPI 文件不是可任意跨机器移植的明文恢复凭据。备份不能只留在同一命名卷或 Docker 数据盘中。

应用备份不是运行中卷的随意文件复制；全卷快照应先停止相关服务、保证一致性。详见 [备份范围与排除项](https://docs.gitlab.com/administration/backup_restore/backup_gitlab/)。升级前保存确切版本和可恢复备份，遵循 GitLab 升级路径，并定期在隔离环境验证恢复。GitHub 代码副本不能替代 GitLab 数据库、项目配置、产物和凭据备份。
