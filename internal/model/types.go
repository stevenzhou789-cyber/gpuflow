package model

import "time"

const (
	LabelAcceleratorVendor  = "accelerator.vendor"
	LabelAcceleratorRuntime = "accelerator.runtime"
	VendorNVIDIA            = "nvidia"
	VendorHuawei            = "huawei"
	RuntimeCUDA             = "cuda"
	RuntimeCANN             = "cann"
)

const (
	// AgentSessionTTL is the server-side takeover threshold.
	AgentSessionTTL = 30 * time.Second
	// AgentSessionFailStopTTL leaves time to force-remove local containers
	// before the control plane may admit a replacement session.
	AgentSessionFailStopTTL = 25 * time.Second
)

type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobAssigned  JobStatus = "assigned"
	JobRunning   JobStatus = "running"
	JobCanceling JobStatus = "canceling"
	JobCanceled  JobStatus = "canceled"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobDeleting  JobStatus = "deleting"
)

type Requirements struct {
	GPUCount  int               `json:"gpu_count"`
	MinVRAMGB int               `json:"min_vram_gb"`
	GPUModels []string          `json:"gpu_models,omitempty"`
	Providers []string          `json:"providers,omitempty"`
	Pools     []string          `json:"pools,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	MaxHourly float64           `json:"max_hourly_price,omitempty"`
}

type Job struct {
	ID              string                   `json:"id"`
	Name            string                   `json:"name"`
	Image           string                   `json:"image"`
	Command         []string                 `json:"command,omitempty"`
	Environment     map[string]string        `json:"environment,omitempty"`
	Requirements    Requirements             `json:"requirements"`
	Strategy        string                   `json:"strategy"`
	TimeoutSeconds  int                      `json:"timeout_seconds"`
	MaxRetries      int                      `json:"max_retries"`
	Attempts        int                      `json:"attempts"`
	Recoveries      int                      `json:"recoveries"`
	Status          JobStatus                `json:"status"`
	AssignedNode    string                   `json:"assigned_node,omitempty"`
	AllocatedGPUs   []int                    `json:"allocated_gpus,omitempty"`
	Output          string                   `json:"output,omitempty"`
	Error           string                   `json:"error,omitempty"`
	CreatedAt       time.Time                `json:"created_at"`
	UpdatedAt       time.Time                `json:"updated_at"`
	StartedAt       *time.Time               `json:"started_at,omitempty"`
	FinishedAt      *time.Time               `json:"finished_at,omitempty"`
	RerunOf         string                   `json:"rerun_of,omitempty"`
	AssignedSession string                   `json:"-"`
	AttemptToken    string                   `json:"-"`
	LeaseExpiresAt  *time.Time               `json:"-"`
	UsageRecords    []AcceleratorUsageRecord `json:"usage_records,omitempty"`
}

// AcceleratorUsageRecord is an immutable pricing snapshot for one execution
// attempt. Enterprise billing persists these records independently of jobs.
type AcceleratorUsageRecord struct {
	Attempt          int        `json:"attempt"`
	NodeID           string     `json:"node_id"`
	Vendor           string     `json:"vendor"`
	Runtime          string     `json:"runtime"`
	Model            string     `json:"model"`
	DeviceCount      int        `json:"device_count"`
	UnitPricePerHour float64    `json:"unit_price_per_device_hour"`
	AssignedAt       time.Time  `json:"assigned_at"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	Status           string     `json:"status"`
}

type JobCreate struct {
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	Command        []string          `json:"command,omitempty"`
	Environment    map[string]string `json:"environment,omitempty"`
	Requirements   Requirements      `json:"requirements"`
	Strategy       string            `json:"strategy,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	MaxRetries     int               `json:"max_retries,omitempty"`
}

type Node struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Provider        string            `json:"provider"`
	Pool            string            `json:"pool"`
	GPUModel        string            `json:"gpu_model"`
	GPUCount        int               `json:"gpu_count"`
	CPUCores        int               `json:"cpu_cores"`
	VRAMGB          int               `json:"vram_gb"`
	HourlyPrice     float64           `json:"hourly_price"`
	Labels          map[string]string `json:"labels,omitempty"`
	Busy            bool              `json:"busy"`
	CurrentJob      string            `json:"current_job,omitempty"`
	ActiveJobs      []string          `json:"active_jobs,omitempty"`
	AllocatedGPUs   int               `json:"allocated_gpu_count"`
	Devices         []GPUDevice       `json:"devices,omitempty"`
	DriverVersion   string            `json:"driver_version,omitempty"`
	DockerVersion   string            `json:"docker_version,omitempty"`
	HealthStatus    string            `json:"health_status,omitempty"`
	HealthReason    string            `json:"health_reason,omitempty"`
	LastHealthCheck *time.Time        `json:"last_health_check,omitempty"`
	LastHeartbeat   time.Time         `json:"last_heartbeat"`
	CleanupPending  bool              `json:"cleanup_pending,omitempty"`
	SessionEpoch    string            `json:"-"`
}

const (
	HeaderAgentSession = "X-GPUFlow-Agent-Session"
	HeaderAttemptToken = "X-GPUFlow-Attempt-Token"
)

// AgentJob is the private task-dispatch representation. AttemptToken is only
// returned by the node polling endpoint and is never exposed by user job APIs.
type AgentJob struct {
	Job
	AttemptToken string `json:"attempt_token"`
}

type GPUDevice struct {
	Index  int    `json:"index"`
	UUID   string `json:"uuid"`
	Model  string `json:"model"`
	VRAMGB int    `json:"vram_gb"`
}

type NodeHealthUpdate struct {
	Status        string      `json:"status"`
	Reason        string      `json:"reason,omitempty"`
	Devices       []GPUDevice `json:"devices,omitempty"`
	GPUModel      string      `json:"gpu_model,omitempty"`
	GPUCount      int         `json:"gpu_count"`
	VRAMGB        int         `json:"vram_gb"`
	DriverVersion string      `json:"driver_version,omitempty"`
	DockerVersion string      `json:"docker_version,omitempty"`
}

type JobUpdate struct {
	Status JobStatus `json:"status"`
	Output string    `json:"output,omitempty"`
	Error  string    `json:"error,omitempty"`
}

type JobLogUpdate struct {
	Output string `json:"output"`
}

type TaskImage struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Runtime   string    `json:"runtime"`
	BaseImage string    `json:"base_image"`
	Filename  string    `json:"filename"`
	Command   string    `json:"command"`
	Status    string    `json:"status"`
	Log       string    `json:"log,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
