# CPU 与主机内存控制

任务的 `requirements.cpu_cores` 和 `requirements.memory_mib` 同时表示调度预留量和 Docker 运行上限。例如：

```json
{
  "name": "training",
  "image": "registry.example/training:v1",
  "requirements": {
    "gpu_count": 1,
    "cpu_cores": 4,
    "memory_mib": 16384
  }
}
```

- CPU 允许 0 或 0.01–65536 核，精度为 0.001 核。内存允许 0 或 6–1073741824 MiB。
- 新建任务表单默认 1 核、2048 MiB；训练前应根据数据加载、模型与批量大小调整。
- 旧任务、API 未填写的字段和显式 0 保持原行为：该维度不预留、不限制。重新运行保留原任务配置。要对同机并发提供完整的 CPU/内存预算，参与共享的任务都必须填写这两个正值。
- 调度器在同一锁内累计 assigned、running、canceling 任务的预留量，防止同一轮分配超出已知容量。取消后要等 Agent 确认结束才释放；CPU-only 任务仍然整节点独占。
- Agent 使用 `--cpus` 限制 CPU 时间，使用 `--memory` 与等额 `--memory-swap` 限制内存并禁止额外交换空间。CPU 上限不表示绑定某个 CPU 核。内存超限可能触发容器 OOM，任务按失败处理。

Agent 从执行任务的 Docker daemon 查询 CPU、总内存，以及 `CPUCfsQuota`、`MemoryLimit`、`SwapLimit` 支持标志，适用于宿主机、容器化 Agent 和远程 Docker。CPU 报告值还受现有 `--cpu-cores` 配置上限约束。查询失败时报告未知容量（0），不会使用 Agent 容器或客户端机器的内存代替。只有容量有效且三个限制标志都为 true 时，节点才声明 `host_resource_limits` 能力；缺少任一限制支持时保留已知容量用于展示，但不接收正值 CPU 或内存请求。旧 Agent 与 mock executor 同样不能接收设置了正值 CPU 或内存的任务，避免 Docker 仅给出警告却没有执行限制。

主机资源数据通过节点注册、健康上报与 MySQL details JSON 保存。健康上报省略字段兼容旧 Agent；明确上报容量变化时，尚未开始的任务会重新排队，运行任务保持原来的容器限制。节点 API 返回 `memory_mib`、`allocated_cpu_cores` 和 `allocated_memory_mib`，其中 allocated 表示预留量，不是实时利用率。

这些预算只核算 GPUFlow 声明的任务资源，不能隔离宿主机或其他平台启动的进程。生产环境应为操作系统、Docker 和其他服务保留足够余量，并监控主机压力。现有 `--shm-size` 默认值未改变；这次不增加数据卷或共享内存配置。
