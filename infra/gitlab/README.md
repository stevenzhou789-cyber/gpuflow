# GPUFlow 本地 GitLab 运维

本配置用于 Windows Docker Desktop（WSL2 / Linux 容器）的**本机开发验证**，不是生产部署或团队高可用代码服务器。它把社区版、企业版、许可证工具三个私有项目放入本地 GitLab，并使用本地磁盘保存构建产物。保留 GitHub 远端；本机停机、休眠、Docker Desktop 关闭或磁盘故障都会影响 GitLab。

## 地址与资源

| 服务 | 本机访问 | 宿主绑定 |
| --- | --- | --- |
| GitLab Web / HTTP Git | `http://gitlab.gpuflow.test:8088` | `127.0.0.1:8088` |
| Git SSH | `ssh://git@gitlab.gpuflow.test:2224` | `127.0.0.1:2224` |
| GitLab Container Registry | `gitlab.gpuflow.test:5055` | `127.0.0.1:5055` |

这三个端口仅绑定回环地址，不对局域网或公网开放。Docker 内网使用 `gpuflow-devops` / `172.30.88.0/24`，GitLab 为 `.2`、builder 为 `.3`；如与 VPN/已有网络冲突，应先审查并一致调整 Compose 和 Runner 配置，不能只改一处。

容器上限：GitLab 3.5 GiB / 4 CPU，builder 2 GiB / 2 CPU，Runner 256 MiB / 1 CPU。Runner 只串行执行一个作业，单作业上限 1.5 GiB / 2 CPU；Go 编译并发为 2，Node 堆上限 768 MiB。还需给 Windows、WSL2 和 Docker 留余量；这些是低资源开发配置，不代表 GitLab 官方生产规格。发生 OOM 时先检查资源、减少同时运行的其他容器，不要反复加大并发。

## 首次启动

以下命令从社区版仓库根目录 `D:\workspace\gpuflow` 执行。先打开 Docker Desktop，确认正在运行 Linux 容器，且 `docker info` 成功。

使用管理员权限的编辑器手动编辑 `C:\Windows\System32\drivers\etc\hosts`，在没有冲突记录时添加一行：

```text
127.0.0.1 gitlab.gpuflow.test
```

脚本不会自动取得管理员权限或改写 hosts。浏览器直接访问 `127.0.0.1:8088` 可能遇到与规范域名不同的跳转；请完成 hosts 后使用上表域名。

```powershell
docker compose -f .\infra\gitlab\compose.yaml up -d gitlab
docker compose -f .\infra\gitlab\compose.yaml ps
```

GitLab 首次初始化可能需要数分钟。等 GitLab 为 healthy，或本机 readiness 返回成功后，再执行初始化与 Runner 注册：

```powershell
docker exec gpuflow-gitlab curl --fail --silent http://127.0.0.1:8088/-/readiness
powershell -NoProfile -File .\infra\gitlab\bootstrap.ps1
powershell -NoProfile -File .\infra\gitlab\register-runner.ps1
docker compose -f .\infra\gitlab\compose.yaml ps
```

`bootstrap.ps1` 关闭公开注册、校验三个项目均私有、配置企业版读取固定社区提交的 Job Token allowlist、创建受限 Git 推送凭据与 group Runner。`register-runner.ps1` 启动 builder 并注册 `linux-docker` Runner。应以同一 Windows 用户运行；不要把脚本改成向任意外部 API 地址发送本地凭据。

项目路径：`gpuflow/gpuflow`、`gpuflow/gpuflow-enterprise`、`gpuflow/gpuflow-license-issuer`。源码同步并不会把工作区未跟踪的 `.env`、密钥、许可证或缓存加入 Git。

## 登录与凭据

初始管理员用户名为 `root`。初始化脚本将登录信息保存在仓库根目录 `.local-gitlab/admin-login.credential.xml`，通过 Windows DPAPI 加密并限制目录 ACL；一般仅创建它的 Windows 用户在原机器上能解密。该目录已被 Git 忽略，不能上传或作为 CI artifact。

如需登录，可由你自己在本机、未开启录屏或 PowerShell transcript 的终端执行以下命令。**命令会显示密码：不要把结果粘贴到聊天、工单、日志、文档或截图。**

```powershell
(Import-Clixml -LiteralPath .\.local-gitlab\admin-login.credential.xml).GetNetworkCredential().Password
```

不要在 CI 中执行此命令。不要用 root 密码作为 CI 变量，也不要打印其他 `*.credential.xml` 内容。初始密码文件可能被 GitLab 清除；已生成的本地加密登录记录仍可用，但更换 Windows 用户/重装机器后不能假定仍能解密。管理员改密码后要自行更新安全保存方式；不要盲目重置已有账号。

初始化 API、Runner、Git 推送凭据与 root 登录密码用途不同。Git 推送令牌有有效期，过期应受控轮换；不能将所有凭据统一换成一个长期管理员令牌。

## CI 信任边界与产物

Runner 和作业都不挂载宿主机 `/var/run/docker.sock`。Runner 通过内部 `tcp://builder:2375` 使用独立 Docker-in-Docker daemon；该 Docker API 不发布宿主端口。**DinD builder 自身仍是 privileged 容器，不是强多租户安全沙箱**。仅允许运行这三个可信仓库经审查的代码；不要开放公开注册、任意 fork/MR、外部贡献者或其他不可信项目到此 Runner。

Registry 当前使用本机 HTTP。builder 已为 `gitlab.gpuflow.test:5055` 配置 insecure registry。若你要用宿主 Docker Desktop 自己拉这些镜像，需要在 Docker Engine 设置中仅为这个本机 Registry 配置对应地址；不要关闭全局 TLS 验证，也不要把 HTTP Registry 暴露到公网。

GitLab 当前输出 **development / unsigned** 产物，不能冒充正式 `v*` 发布。GitHub 的正式签名、安全扫描/报告留存、风险批准和其他发布门禁保持原样；GitLab tag 流水线在门禁未迁移前明确失败，不移动已有 `v1.1.3` tag。许可证签发工具只构建原生二进制，不容器化，不创建或上传真实签发私钥。

产物保留时间按各项目 `.gitlab-ci.yml` 为准；构建日志需要脱敏并限制项目成员访问。Registry 镜像有独立生命周期，应配置开发镜像保留/清理规则并定期查看磁盘使用。不要删除正在部署或仍需回滚的 Digest。完整迁移正式发布链是单独工作，不能因本地开发构建成功就宣布正式发行成功。

## 日常双远端同步

保留每个仓库现有 GitHub `origin`，另设一个 GitLab `gitlab` 远端；每个远端仅一个推送 URL。提交前审查 diff、运行测试、明确选择源码文件，不要无差别加入密钥或缓存。完成本地 commit 后，从社区版仓库调用：

```powershell
powershell -NoProfile -File .\scripts\push-all.ps1 -RepoPath D:\workspace\gpuflow
powershell -NoProfile -File .\scripts\push-all.ps1 -RepoPath D:\workspace\gpuflow-commercial
powershell -NoProfile -File .\scripts\push-all.ps1 -RepoPath D:\workspace\gpuflow-license-issuer
```

这些是原工作区路径示例；如果仍在隔离迁移副本工作，应传入该仓库实际路径，不能混用。脚本不替你 stage/commit、创建 tag 或 force-push；它会检查远端并推送同一份已有提交。需要同步已存在且经过批准的 tag 时才显式使用 `-Tag <已有tag>`。

GitHub 与 GitLab 是两个独立服务，**双推不是跨服务器原子事务**。若一个成功、另一个失败，检查脚本报告并在修复连接/权限后补推同一 SHA；不要为“保持一致”移动已有正式 tag、重写远端历史或做强制推送。

此机器三个原始仓库还配置了仅针对本机域名的 Git 解析和代理例外：`http.curloptResolve=gitlab.gpuflow.test:8088:127.0.0.1`、`http.http://gitlab.gpuflow.test:8088.proxy` 为空。因此 Git 同步不依赖 Windows hosts 是否已更新，也不会把本地请求发往网络代理。这不影响浏览器，网页仍需 hosts 映射；若以后迁移 GitLab 到其他机器，应先修改这些仓库级配置。

## 停止、重启、备份

日常暂停使用 `stop`，恢复使用 `up -d`，两者保留命名卷：

```powershell
docker compose -f .\infra\gitlab\compose.yaml stop
docker compose -f .\infra\gitlab\compose.yaml up -d
```

不要执行 `docker compose down -v`、`docker volume prune`、`docker system prune --volumes` 或 Docker Desktop 的清空/恢复出厂设置来“修复构建”。这些操作可能永久删除代码、数据库、镜像、CI 产物和密钥。普通容器重建也不是备份。

需保护的命名卷：

- `gpuflow-gitlab-data`：数据库、仓库、应用数据、产物、Registry 数据等。
- `gpuflow-gitlab-config`：GitLab 配置、`gitlab-secrets.json` 及相关密钥。
- `gpuflow-gitlab-logs`：服务日志，注意敏感信息与保留时间。
- `gpuflow-gitlab-runner-config`：Runner 注册配置与认证信息。
- `gpuflow-gitlab-builder-data`：构建 daemon 的镜像和缓存；即使可重建，也必须辨认目标后才清理。

先暂停新的 CI 作业并等待在跑任务结束，再按 [GitLab Docker 备份说明](https://docs.gitlab.com/install/docker/backup/) 生成应用备份：

```powershell
docker exec gpuflow-gitlab gitlab-backup create
```

备份归档默认在容器 `/var/opt/gitlab/backups`。还需**单独备份** `/etc/gitlab`（特别是 `gitlab-secrets.json`）、本目录 Compose 配置与 Runner 配置；将备份复制到 Docker 数据盘之外的受控位置并限制访问，不能只留在同一个命名卷里。GitLab 应用备份不是运行中命名卷的随意文件复制；若选择全卷快照，应先停止有关服务，保证一致性。详见 [备份范围与排除项](https://docs.gitlab.com/administration/backup_restore/backup_gitlab/)。

升级前保留当前确切 GitLab 版本和可恢复备份，遵循 GitLab 升级路径；备份恢复要核对版本兼容性。定期在隔离环境验证恢复。双远端只能提供源码的另一份副本，不能替代 GitLab 数据库、项目设置、构建产物与凭据的备份。
