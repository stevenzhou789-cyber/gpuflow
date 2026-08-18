import { FormEvent, useCallback, useEffect, useMemo, useState } from "react";

type JobStatus = "queued" | "assigned" | "running" | "succeeded" | "failed";

type Job = {
  id: string;
  name: string;
  image: string;
  strategy: string;
  status: JobStatus;
  attempts: number;
  max_retries: number;
  assigned_node?: string;
  error?: string;
  output?: string;
  created_at: string;
  requirements: { gpu_count: number; min_vram_gb: number; pools?: string[] };
};

type Node = {
  id: string;
  name: string;
  provider: string;
  pool: string;
  gpu_model: string;
  gpu_count: number;
  vram_gb: number;
  hourly_price: number;
  busy: boolean;
  current_job?: string;
  last_heartbeat: string;
};

type Edition = {
  name: string;
  licensed_to?: string;
  expires_at?: string;
  agent_image?: string;
  features: Record<string, boolean>;
};

type Insights = {
  completed_jobs: number;
  retry_count: number;
  average_runtime_minutes: number;
  recommendation: string;
};

type TaskImageStatus = "building" | "ready" | "failed";

type TaskImage = {
  id: string;
  name: string;
  runtime: string;
  base_image: string;
  filename: string;
  command: string;
  status: TaskImageStatus;
  log?: string;
  error?: string;
  created_at: string;
  updated_at: string;
};

type Page = "overview" | "jobs" | "images" | "nodes";

const statusLabel: Record<JobStatus, string> = {
  queued: "排队中",
  assigned: "已分配",
  running: "运行中",
  succeeded: "已完成",
  failed: "失败",
};

const nav: { id: Page; label: string; mark: string }[] = [
  { id: "overview", label: "运行概览", mark: "◫" },
  { id: "jobs", label: "任务队列", mark: "≡" },
  { id: "images", label: "任务镜像", mark: "▣" },
  { id: "nodes", label: "算力节点", mark: "◇" },
];

function requestHeaders() {
  const token = sessionStorage.getItem("gpuflow_token") || "";
  return {
    "Content-Type": "application/json",
    ...(token ? { Authorization: `Bearer ${token}` } : {}),
  };
}

async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const headers: Record<string, string> = {
    ...requestHeaders(),
    ...(init?.headers as Record<string, string> | undefined),
  };
  if (init?.body instanceof FormData) delete headers["Content-Type"];
  const response = await fetch(path, {
    ...init,
    headers,
  });
  if (response.status === 401) throw new Error("AUTH_REQUIRED");
  if (!response.ok) {
    const data = await response
      .json()
      .catch(() => ({ error: response.statusText }));
    throw new Error(data.error || response.statusText);
  }
  if (response.status === 204) return undefined as T;
  return response.json();
}

function timeAgo(value: string) {
  const seconds = Math.max(
    0,
    Math.floor((Date.now() - new Date(value).getTime()) / 1000),
  );
  if (seconds < 60) return `${seconds}秒前`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}分钟前`;
  return `${Math.floor(seconds / 3600)}小时前`;
}

function App() {
  const [page, setPage] = useState<Page>("overview");
  const [jobs, setJobs] = useState<Job[]>([]);
  const [nodes, setNodes] = useState<Node[]>([]);
  const [taskImages, setTaskImages] = useState<TaskImage[]>([]);
  const [edition, setEdition] = useState<Edition>({
    name: "community",
    features: {},
  });
  const [insights, setInsights] = useState<Insights | null>(null);
  const [authRequired, setAuthRequired] = useState(false);
  const [token, setToken] = useState(
    sessionStorage.getItem("gpuflow_token") || "",
  );
  const [showSubmit, setShowSubmit] = useState(false);
  const [showBuild, setShowBuild] = useState(false);
  const [showConnect, setShowConnect] = useState(false);
  const [submitPreset, setSubmitPreset] = useState<{
    image: string;
    command: string;
  } | null>(null);
  const [error, setError] = useState("");

  const refresh = useCallback(async () => {
    try {
      const [nextJobs, nextNodes, nextImages] = await Promise.all([
        api<Job[]>("/v1/jobs"),
        api<Node[]>("/v1/nodes"),
        api<TaskImage[]>("/v1/task-images"),
      ]);
      setJobs(nextJobs);
      setNodes(nextNodes);
      setTaskImages(nextImages);
      setAuthRequired(false);
      setError("");
    } catch (err) {
      if ((err as Error).message === "AUTH_REQUIRED") setAuthRequired(true);
      else setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    api<Edition>("/v1/capabilities")
      .then(setEdition)
      .catch(() => undefined);
    refresh();
    const timer = window.setInterval(refresh, 5000);
    return () => window.clearInterval(timer);
  }, [refresh]);

  useEffect(() => {
    if (edition.features.cost_analytics && !authRequired) {
      api<Insights>("/commercial/v1/insights")
        .then(setInsights)
        .catch(() => undefined);
    }
  }, [edition, authRequired, jobs]);

  const metrics = useMemo(
    () => ({
      active: jobs.filter(
        (job) => job.status === "running" || job.status === "assigned",
      ).length,
      queued: jobs.filter((job) => job.status === "queued").length,
      success: jobs.length
        ? Math.round(
            (jobs.filter((job) => job.status === "succeeded").length /
              jobs.length) *
              100,
          )
        : 0,
      online: nodes.filter(
        (node) => Date.now() - new Date(node.last_heartbeat).getTime() < 30_000,
      ).length,
    }),
    [jobs, nodes],
  );

  function saveToken(event: FormEvent) {
    event.preventDefault();
    sessionStorage.setItem("gpuflow_token", token);
    refresh();
  }

  async function deleteNode(node: Node) {
    if (node.busy || node.current_job) return;
    if (
      !window.confirm(`确认删除节点“${node.name}”？已完成任务记录不会被删除。`)
    )
      return;
    try {
      await api<void>(`/v1/nodes/${encodeURIComponent(node.id)}`, {
        method: "DELETE",
      });
      await refresh();
    } catch (err) {
      setError(
        (err as Error).message === "node has an assigned or running job"
          ? "该节点仍有任务正在分配或运行，不能删除。"
          : (err as Error).message,
      );
    }
  }

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand">
          <span className="brand-mark">GF</span>
          <div>
            <strong>GPUFlow</strong>
            <small>CONTROL PLANE</small>
          </div>
        </div>
        <nav>
          {nav.map((item) => (
            <button
              key={item.id}
              className={page === item.id ? "active" : ""}
              onClick={() => setPage(item.id)}
            >
              <span>{item.mark}</span>
              {item.label}
            </button>
          ))}
        </nav>
        <div className="edition-card">
          <span className="eyebrow">CURRENT EDITION</span>
          <strong>{edition.name.toUpperCase()}</strong>
          <p>{edition.licensed_to || "Open-source control plane"}</p>
        </div>
      </aside>

      <main>
        <header>
          <div>
            <p className="eyebrow">GPU ORCHESTRATION</p>
            <h1>{nav.find((item) => item.id === page)?.label}</h1>
          </div>
          <div className="header-actions">
            <span className="live">
              <i />
              实时同步
            </span>
            {page === "nodes" ? (
              <button className="primary" onClick={() => setShowConnect(true)}>
                ＋ 接入节点
              </button>
            ) : page === "images" ? (
              <button className="primary" onClick={() => setShowBuild(true)}>
                ＋ 构建镜像
              </button>
            ) : (
              <button
                className="primary"
                onClick={() => {
                  setSubmitPreset(null);
                  setShowSubmit(true);
                }}
              >
                ＋ 提交任务
              </button>
            )}
          </div>
        </header>

        {error && <div className="notice error">无法刷新数据：{error}</div>}
        {page === "overview" && (
          <Overview
            metrics={metrics}
            jobs={jobs}
            nodes={nodes}
            edition={edition}
            insights={insights}
            onNavigate={setPage}
          />
        )}
        {page === "jobs" && <Jobs jobs={jobs} />}
        {page === "images" && (
          <TaskImages
            images={taskImages}
            onBuild={() => setShowBuild(true)}
            onSubmit={(image) => {
              setSubmitPreset({ image: image.name, command: image.command });
              setShowSubmit(true);
            }}
          />
        )}
        {page === "nodes" && (
          <Nodes
            nodes={nodes}
            onConnect={() => setShowConnect(true)}
            onDelete={deleteNode}
          />
        )}
      </main>

      {authRequired && (
        <div className="modal-backdrop">
          <form className="modal auth" onSubmit={saveToken}>
            <span className="brand-mark">GF</span>
            <h2>连接控制面</h2>
            <p>输入部署时配置的访问 Token。Token 仅保存在当前浏览器会话中。</p>
            <label>
              访问 Token
              <input
                type="password"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                autoFocus
              />
            </label>
            <button className="primary" type="submit">
              连接 GPUFlow
            </button>
          </form>
        </div>
      )}
      {showSubmit && (
        <SubmitJob
          nodes={nodes}
          initialImage={submitPreset?.image}
          initialCommand={submitPreset?.command}
          onClose={() => setShowSubmit(false)}
          onCreated={() => {
            setShowSubmit(false);
            setPage("jobs");
            refresh();
          }}
        />
      )}
      {showBuild && (
        <BuildTaskImage
          onClose={() => setShowBuild(false)}
          onCreated={() => {
            setShowBuild(false);
            setPage("images");
            refresh();
          }}
        />
      )}
      {showConnect && (
        <ConnectNode
          nodes={nodes}
          defaultAgentImage={edition.agent_image || "gpuflow:latest"}
          onClose={() => setShowConnect(false)}
        />
      )}
    </div>
  );
}

function Overview({
  metrics,
  jobs,
  nodes,
  edition,
  insights,
  onNavigate,
}: {
  metrics: { active: number; queued: number; success: number; online: number };
  jobs: Job[];
  nodes: Node[];
  edition: Edition;
  insights: Insights | null;
  onNavigate: (page: Page) => void;
}) {
  const recent = jobs.slice(0, 5);
  return (
    <>
      <section className="metrics">
        <Metric
          label="运行中任务"
          value={metrics.active}
          note={`${metrics.queued} 个等待调度`}
          tone="green"
        />
        <Metric
          label="在线节点"
          value={`${metrics.online}/${nodes.length}`}
          note={`${nodes.reduce((sum, node) => sum + node.gpu_count, 0)} 张 GPU 已接入`}
          tone="blue"
        />
        <Metric
          label="任务成功率"
          value={`${metrics.success}%`}
          note={`基于 ${jobs.length} 次执行`}
          tone="amber"
        />
        <Metric
          label="平均运行时长"
          value={
            edition.features.cost_analytics
              ? `${(insights?.average_runtime_minutes || 0).toFixed(1)}m`
              : "Pro"
          }
          note={
            edition.features.cost_analytics
              ? insights?.recommendation || "正在生成运行洞察"
              : "升级后查看成本洞察"
          }
          tone="purple"
        />
      </section>
      <section className="grid-main">
        <div className="panel">
          <PanelTitle
            title="最近任务"
            action="查看全部"
            onAction={() => onNavigate("jobs")}
          />
          {recent.length ? (
            <div className="job-list">
              {recent.map((job) => (
                <JobRow key={job.id} job={job} />
              ))}
            </div>
          ) : (
            <Empty
              title="还没有任务"
              text="提交第一个容器任务，GPUFlow 会为它选择合适节点。"
            />
          )}
        </div>
        <div className="panel capacity">
          <PanelTitle
            title="资源池"
            action="管理节点"
            onAction={() => onNavigate("nodes")}
          />
          {nodes.length ? (
            nodes.slice(0, 4).map((node) => (
              <div className="capacity-row" key={node.id}>
                <div>
                  <strong>{node.name}</strong>
                  <span>
                    {node.provider} · {node.pool}
                  </span>
                </div>
                <div className="capacity-meta">
                  <b>{node.gpu_count}×</b>
                  <span>{node.gpu_model}</span>
                </div>
              </div>
            ))
          ) : (
            <Empty
              title="尚未接入节点"
              text="启动 Agent 后，节点会自动出现在这里。"
            />
          )}
        </div>
      </section>
      <section className="feature-strip">
        <div>
          <span className="eyebrow">NEXT LEVEL</span>
          <h3>从任务队列走向成本自治</h3>
          <p>
            高级策略会综合时价、显存、可靠性与完成期限，而不仅仅选择最便宜的节点。
          </p>
        </div>
        <div className="feature-tags">
          <span className={edition.features.advanced_policy ? "enabled" : ""}>
            高级调度
          </span>
          <span className={edition.features.alerts ? "enabled" : ""}>
            异常告警
          </span>
          <span className={edition.features.rbac ? "enabled" : ""}>
            团队权限
          </span>
          <span className={edition.features.audit_log ? "enabled" : ""}>
            审计日志
          </span>
        </div>
      </section>
    </>
  );
}

function Metric({
  label,
  value,
  note,
  tone,
}: {
  label: string;
  value: string | number;
  note: string;
  tone: string;
}) {
  return (
    <div className={`metric ${tone}`}>
      <span>{label}</span>
      <strong>{value}</strong>
      <small>{note}</small>
    </div>
  );
}
function PanelTitle({
  title,
  action,
  onAction,
}: {
  title: string;
  action: string;
  onAction: () => void;
}) {
  return (
    <div className="panel-title">
      <h2>{title}</h2>
      <button onClick={onAction}>{action} →</button>
    </div>
  );
}
function Empty({ title, text }: { title: string; text: string }) {
  return (
    <div className="empty">
      <span>⌁</span>
      <strong>{title}</strong>
      <p>{text}</p>
    </div>
  );
}

function JobRow({ job }: { job: Job }) {
  return (
    <div className="job-row">
      <div className="job-icon">{job.requirements.gpu_count || "CPU"}</div>
      <div className="job-main">
        <strong>{job.name}</strong>
        <span>{job.image}</span>
      </div>
      <div className="job-node">
        <span>{job.assigned_node || "等待节点"}</span>
        <small>{timeAgo(job.created_at)}</small>
      </div>
      <span className={`status ${job.status}`}>{statusLabel[job.status]}</span>
    </div>
  );
}

function Jobs({ jobs }: { jobs: Job[] }) {
  return (
    <section className="panel table-panel">
      <div className="panel-title">
        <div>
          <h2>全部任务</h2>
          <p>{jobs.length} 个任务记录</p>
        </div>
      </div>
      {jobs.length ? (
        <div className="job-list">
          {jobs.map((job) => (
            <JobRow key={job.id} job={job} />
          ))}
        </div>
      ) : (
        <Empty title="任务队列为空" text="点击右上角提交第一个任务。" />
      )}
    </section>
  );
}

const imageStatusLabel: Record<TaskImageStatus, string> = {
  building: "构建中",
  ready: "可用",
  failed: "失败",
};

function TaskImages({
  images,
  onBuild,
  onSubmit,
}: {
  images: TaskImage[];
  onBuild: () => void;
  onSubmit: (image: TaskImage) => void;
}) {
  return (
    <>
      <section className="connect-banner image-banner">
        <div>
          <span className="eyebrow">SCRIPT TO CONTAINER</span>
          <h2>上传脚本，自动构建任务镜像</h2>
          <p>适用于控制端与 Agent 共用 Docker 的本机环境；远程节点需要共享镜像仓库。</p>
        </div>
        <button onClick={onBuild}>上传任务脚本 →</button>
      </section>
      <section className="image-grid">
        {images.map((image) => (
          <article className="image-card" key={image.id}>
            <div className="image-card-head">
              <span className={`status ${image.status}`}>
                {imageStatusLabel[image.status]}
              </span>
              <small>{timeAgo(image.created_at)}</small>
            </div>
            <h2>{image.name}</h2>
            <p>{image.base_image}</p>
            <dl>
              <div><dt>脚本</dt><dd>{image.filename}</dd></div>
              <div><dt>入口</dt><dd>{image.command}</dd></div>
            </dl>
            {image.error && <div className="notice error">{image.error}</div>}
            {image.log && (
              <details>
                <summary>构建日志</summary>
                <pre>{image.log}</pre>
              </details>
            )}
            <button
              className="primary"
              disabled={image.status !== "ready"}
              onClick={() => onSubmit(image)}
            >
              使用此镜像提交任务
            </button>
          </article>
        ))}
        {!images.length && (
          <div className="panel image-empty">
            <Empty title="还没有任务镜像" text="上传 .py 或 .sh 脚本创建第一个任务镜像。" />
          </div>
        )}
      </section>
    </>
  );
}

function Nodes({
  nodes,
  onConnect,
  onDelete,
}: {
  nodes: Node[];
  onConnect: () => void;
  onDelete: (node: Node) => void;
}) {
  return (
    <>
      <section className="connect-banner">
        <div>
          <span className="eyebrow">BRING YOUR OWN COMPUTE</span>
          <h2>把已有 GPU 接入统一队列</h2>
          <p>Agent 在算力节点本地执行容器，控制面只负责调度和状态管理。</p>
        </div>
        <button onClick={onConnect}>查看接入方式 →</button>
      </section>
      <section className="node-grid">
        {nodes.map((node) => {
          const online =
            Date.now() - new Date(node.last_heartbeat).getTime() < 30_000;
          const locked = node.busy || Boolean(node.current_job);
          return (
            <article className="node-card" key={node.id}>
              <div className="node-head">
                <span className={`node-state ${online ? "online" : ""}`}>
                  {online ? "ONLINE" : "OFFLINE"}
                </span>
                <div>
                  <span>{node.provider}</span>
                  <button
                    className="delete-node"
                    disabled={locked}
                    title={locked ? "节点有任务运行，不能删除" : "删除节点"}
                    onClick={() => onDelete(node)}
                  >
                    删除
                  </button>
                </div>
              </div>
              <h2>{node.name}</h2>
              <p>{node.pool}</p>
              <div className="gpu-spec">
                <strong>
                  {node.gpu_count} × {node.gpu_model}
                </strong>
                <span>{node.vram_gb} GB VRAM</span>
              </div>
              <div className="node-footer">
                <span>{node.busy ? `执行 ${node.current_job}` : "空闲"}</span>
                <strong>
                  ¥{node.hourly_price.toFixed(2)}
                  <small>/GPU小时</small>
                </strong>
              </div>
            </article>
          );
        })}
        {!nodes.length && (
          <div className="panel">
            <Empty
              title="没有算力节点"
              text="点击接入节点，复制命令到GPU主机运行。"
            />
          </div>
        )}
      </section>
    </>
  );
}

const gpuProfiles = [
  { value: "RTX-3050-Laptop", label: "RTX 3050 Laptop · 4GB", vram: "4" },
  { value: "RTX-3060", label: "RTX 3060 · 12GB", vram: "12" },
  { value: "RTX-3090", label: "RTX 3090 · 24GB", vram: "24" },
  { value: "RTX-4090", label: "RTX 4090 · 24GB", vram: "24" },
  { value: "NVIDIA-L4", label: "NVIDIA L4 · 24GB", vram: "24" },
  { value: "NVIDIA-L40S", label: "NVIDIA L40S · 48GB", vram: "48" },
  { value: "NVIDIA-A10", label: "NVIDIA A10 · 24GB", vram: "24" },
  { value: "NVIDIA-A100-40GB", label: "NVIDIA A100 · 40GB", vram: "40" },
  { value: "NVIDIA-A100-80GB", label: "NVIDIA A100 · 80GB", vram: "80" },
  { value: "NVIDIA-H100-80GB", label: "NVIDIA H100 · 80GB", vram: "80" },
];

function ConnectNode({
  nodes,
  defaultAgentImage,
  onClose,
}: {
  nodes: Node[];
  defaultAgentImage: string;
  onClose: () => void;
}) {
  const [mode, setMode] = useState<"windows" | "docker">("windows");
  const [copied, setCopied] = useState(false);
  const [agentImage, setAgentImage] = useState(defaultAgentImage);
  const [values, setValues] = useState({
    name: "local-gpu-01",
    pool: "default",
    model: "RTX-4090",
    count: "1",
    vram: "24",
    price: "0",
  });
  const pools = [
    ...new Set([
      "default",
      "local",
      "development",
      ...nodes.map((node) => node.pool),
    ]),
  ];
  const server = window.location.origin;
  const token = sessionStorage.getItem("gpuflow_token") || "";
  const windowsCommand = `${token ? `$env:GPUFLOW_TOKEN = "${token}"\n` : ""}.\\gpuflow.exe agent -server "${server}" -id "${values.name}" -name "${values.name}" -provider local -pool "${values.pool}" -gpu-model "${values.model}" -gpu-count ${values.count} -vram ${values.vram} -hourly-price ${values.price} -executor docker`;
  const dockerCommand = `docker run -d --name gpuflow-agent --restart unless-stopped \\\n+  -v /var/run/docker.sock:/var/run/docker.sock \\\n+  ${agentImage.trim() || "gpuflow:latest"} agent -server "${server}" ${token ? `-token "${token}" ` : ""}-id "${values.name}" -name "${values.name}" \\\n+  -provider local -pool "${values.pool}" -gpu-model "${values.model}" -gpu-count ${values.count} -vram ${values.vram} -hourly-price ${values.price}`;
  const command = (mode === "windows" ? windowsCommand : dockerCommand).replace(
    /\n\+\s*/g,
    "\n  ",
  );

  function update(key: keyof typeof values, value: string) {
    setValues((current) => ({ ...current, [key]: value }));
    setCopied(false);
  }
  function updateModel(value: string) {
    const profile = gpuProfiles.find((item) => item.value === value);
    setValues((current) => ({
      ...current,
      model: value,
      vram: profile?.vram || current.vram,
    }));
    setCopied(false);
  }
  async function copyCommand() {
    await navigator.clipboard.writeText(command);
    setCopied(true);
  }

  return (
    <div className="modal-backdrop">
      <div className="modal connect">
        <div className="modal-title">
          <div>
            <span className="eyebrow">CONNECT COMPUTE</span>
            <h2>接入算力节点</h2>
          </div>
          <button type="button" onClick={onClose} aria-label="关闭">
            ×
          </button>
        </div>
        <div className="connect-modes">
          <button
            className={mode === "windows" ? "active" : ""}
            onClick={() => setMode("windows")}
          >
            Windows Agent
          </button>
          <button
            className={mode === "docker" ? "active" : ""}
            onClick={() => setMode("docker")}
          >
            Linux / Docker
          </button>
          <button disabled>阿里云 · 即将支持</button>
          <button disabled>腾讯云 · 即将支持</button>
        </div>
        {mode === "docker" && (
          <label className="agent-image-field">
            Agent 镜像
            <input
              value={agentImage}
              onChange={(event) => {
                setAgentImage(event.target.value);
                setCopied(false);
              }}
              placeholder="ghcr.io/owner/gpuflow:0.1.0"
            />
            <small>
              正式环境建议使用 GHCR 固定版本；本地构建可使用
              gpuflow:latest。
            </small>
          </label>
        )}
        <div className="form-grid compact">
          <label>
            节点标识（唯一）
            <input
              value={values.name}
              onChange={(event) => update("name", event.target.value)}
            />
            <small>
              用于唯一识别节点，接入后请保持不变。示例：local-rtx3050-01
            </small>
          </label>
          <label>
            资源池
            <select
              value={values.pool}
              onChange={(event) => update("pool", event.target.value)}
            >
              {pools.map((pool) => (
                <option key={pool}>{pool}</option>
              ))}
            </select>
          </label>
          <label>
            GPU 型号
            <select
              value={values.model}
              onChange={(event) => updateModel(event.target.value)}
            >
              {gpuProfiles.map((profile) => (
                <option key={profile.value} value={profile.value}>
                  {profile.label}
                </option>
              ))}
            </select>
          </label>
          <label>
            GPU 数量
            <select
              value={values.count}
              onChange={(event) => update("count", event.target.value)}
            >
              {["1", "2", "4", "8"].map((count) => (
                <option key={count}>{count}</option>
              ))}
            </select>
          </label>
          <label>
            显存容量
            <select
              value={values.vram}
              onChange={(event) => update("vram", event.target.value)}
            >
              {["4", "8", "12", "16", "24", "32", "40", "48", "80"].map(
                (vram) => (
                  <option key={vram} value={vram}>
                    {vram} GB
                  </option>
                ),
              )}
            </select>
          </label>
          <label>
            调度参考单价（可选）
            <div className="input-with-unit">
              <input
                type="number"
                min="0"
                step="0.01"
                value={values.price}
                onChange={(event) => update("price", event.target.value)}
              />
              <span>元 / GPU小时</span>
            </div>
            <small>本机测试保持 0；最低成本策略会优先选择低价节点。</small>
          </label>
        </div>
        <div className="command-block">
          <div>
            <span>在目标 GPU 主机运行</span>
            <button onClick={copyCommand}>
              {copied ? "已复制" : "复制命令"}
            </button>
          </div>
          <pre>{command}</pre>
        </div>
        <div className="connect-note">
          <strong>当前接入边界</strong>
          <p>
            节点必须已安装 Docker；GPU 容器还需要 NVIDIA 驱动和 NVIDIA Container
            Toolkit。Agent 会在宿主机执行任务，只应连接可信控制面。
          </p>
        </div>
      </div>
    </div>
  );
}

function BuildTaskImage({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: () => void;
}) {
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const defaultTag = `gpuflow-task/script:${new Date()
    .toISOString()
    .replace(/[-:T]/g, "")
    .slice(0, 14)}`;

  async function build(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSaving(true);
    setError("");
    try {
      const values = new FormData(event.currentTarget);
      await api<TaskImage>("/v1/task-images/build", {
        method: "POST",
        body: values,
      });
      onCreated();
    } catch (err) {
      setError((err as Error).message);
      setSaving(false);
    }
  }

  return (
    <div className="modal-backdrop">
      <form className="modal build-image" onSubmit={build}>
        <div className="modal-title">
          <div>
            <span className="eyebrow">BUILD TASK IMAGE</span>
            <h2>从脚本构建任务镜像</h2>
          </div>
          <button type="button" onClick={onClose} aria-label="关闭">×</button>
        </div>
        {error && <div className="notice error">{error}</div>}
        <div className="form-grid">
          <label className="wide">
            任务脚本
            <input name="script" type="file" accept=".py,.sh" required />
            <small>支持 Python 或 Shell 单文件，最大 5 MB。</small>
          </label>
          <label>
            运行环境
            <select name="runtime" defaultValue="pytorch">
              <option value="shell">Shell · Node Alpine</option>
              <option value="python">Python 3.12 · CPU</option>
              <option value="pytorch">PyTorch 2.1 · CUDA 12.1</option>
              <option value="cuda12">CUDA 12.0 · Shell</option>
            </select>
          </label>
          <label>
            镜像名称
            <input name="image" defaultValue={defaultTag} required />
          </label>
          <label className="wide">
            Python 依赖（可选）
            <textarea
              name="requirements"
              rows={5}
              placeholder={"numpy==2.1.3\ntransformers==4.46.2"}
            />
            <small>每行一个 pip 依赖；Shell/CUDA 环境请留空。</small>
          </label>
        </div>
        <div className="connect-note">
          <strong>镜像可见范围</strong>
          <p>镜像构建在控制端 Docker 中，仅与控制端共用 Docker 的 Agent 可直接运行。远程节点需要先推送到共享仓库。</p>
        </div>
        <div className="modal-actions">
          <button type="button" onClick={onClose}>取消</button>
          <button className="primary" disabled={saving}>
            {saving ? "正在创建构建…" : "开始构建"}
          </button>
        </div>
      </form>
    </div>
  );
}

function SubmitJob({
  nodes,
  initialImage,
  initialCommand,
  onClose,
  onCreated,
}: {
  nodes: Node[];
  initialImage?: string;
  initialCommand?: string;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [imagePreset, setImagePreset] = useState(
    initialImage ? "custom" : "node:22-alpine",
  );
  const [commandPreset, setCommandPreset] = useState(
    initialCommand ? "custom" : "test -c /dev/dxg && echo GPU_DEVICE_AVAILABLE",
  );
  const pools = [...new Set(nodes.map((node) => node.pool))];
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSaving(true);
    setError("");
    const values = new FormData(event.currentTarget);
    const command = String(values.get("command") || "").trim();
    const pool = String(values.get("pool") || "");
    try {
      const image =
        imagePreset === "custom"
          ? String(values.get("custom_image") || "").trim()
          : imagePreset;
      const selectedCommand =
        commandPreset === "custom" ? command : commandPreset;
      if (!image) throw new Error("请输入自定义容器镜像");
      await api("/v1/jobs", {
        method: "POST",
        body: JSON.stringify({
          name: values.get("name"),
          image,
          command: selectedCommand ? ["sh", "-c", selectedCommand] : [],
          requirements: {
            gpu_count: Number(values.get("gpu_count")),
            min_vram_gb: Number(values.get("vram")),
            pools: pool ? [pool] : [],
          },
          strategy: values.get("strategy"),
          timeout_seconds: Number(values.get("timeout")),
          max_retries: Number(values.get("retries")),
        }),
      });
      onCreated();
    } catch (err) {
      setError((err as Error).message);
      setSaving(false);
    }
  }
  return (
    <div className="modal-backdrop">
      <form className="modal submit" onSubmit={submit}>
        <div className="modal-title">
          <div>
            <span className="eyebrow">NEW WORKLOAD</span>
            <h2>提交任务</h2>
          </div>
          <button type="button" onClick={onClose} aria-label="关闭">
            ×
          </button>
        </div>
        {error && <div className="notice error">{error}</div>}
        <div className="form-grid">
          <label>
            任务名称
            <input name="name" required placeholder="model-evaluation" />
          </label>
          <label>
            容器镜像
            <select
              value={imagePreset}
              onChange={(event) => setImagePreset(event.target.value)}
            >
              <option value="node:22-alpine">Node Alpine · 设备测试</option>
              <option value="nvidia/cuda:12.0.1-base-ubuntu22.04">
                NVIDIA CUDA 12.0
              </option>
              <option value="pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime">
                PyTorch 2.5.1 CUDA
              </option>
              <option value="custom">自定义镜像…</option>
            </select>
          </label>
          {imagePreset === "custom" && (
            <label className="wide">
              自定义镜像
              <input
                name="custom_image"
                required
                defaultValue={initialImage}
                placeholder="registry/team/image:tag"
              />
            </label>
          )}
          <label className="wide">
            执行模板
            <select
              value={commandPreset}
              onChange={(event) => setCommandPreset(event.target.value)}
            >
              <option value="test -c /dev/dxg && echo GPU_DEVICE_AVAILABLE">
                检查GPU设备
              </option>
              <option value="nvidia-smi">查看NVIDIA状态</option>
              <option
                value={
                  'python -c "import torch; print(torch.cuda.get_device_name(0))"'
                }
              >
                检查PyTorch CUDA
              </option>
              <option value="custom">自定义命令…</option>
            </select>
          </label>
          {commandPreset === "custom" && (
            <label className="wide">
              自定义命令
              <input
                name="command"
                required
                defaultValue={initialCommand}
                placeholder="python evaluate.py"
              />
            </label>
          )}
          <label>
            GPU数量
            <select name="gpu_count" defaultValue="1">
              <option value="0">不使用GPU</option>
              {["1", "2", "4", "8"].map((count) => (
                <option key={count}>{count}</option>
              ))}
            </select>
          </label>
          <label>
            最低显存
            <select name="vram" defaultValue="4">
              <option value="0">不限制</option>
              {["4", "8", "12", "16", "24", "32", "40", "48", "80"].map(
                (vram) => (
                  <option key={vram} value={vram}>
                    {vram} GB
                  </option>
                ),
              )}
            </select>
          </label>
          <label>
            资源池
            <select name="pool" defaultValue="">
              <option value="">任意资源池</option>
              {pools.map((pool) => (
                <option key={pool}>{pool}</option>
              ))}
            </select>
          </label>
          <label>
            调度策略
            <select name="strategy">
              <option value="lowest_cost">最低成本</option>
              <option value="most_vram">最大显存</option>
            </select>
          </label>
          <label>
            任务超时
            <select name="timeout" defaultValue="3600">
              <option value="300">5分钟</option>
              <option value="1800">30分钟</option>
              <option value="3600">1小时</option>
              <option value="14400">4小时</option>
              <option value="43200">12小时</option>
              <option value="86400">24小时</option>
            </select>
          </label>
          <label>
            失败重试
            <select name="retries" defaultValue="1">
              <option value="0">不重试</option>
              <option value="1">1次</option>
              <option value="2">2次</option>
              <option value="3">3次</option>
            </select>
          </label>
        </div>
        <div className="modal-actions">
          <button type="button" onClick={onClose}>
            取消
          </button>
          <button className="primary" disabled={saving}>
            {saving ? "提交中…" : "提交并调度"}
          </button>
        </div>
      </form>
    </div>
  );
}

export default App;
