package model

import "time"

type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobAssigned  JobStatus = "assigned"
	JobRunning   JobStatus = "running"
	JobCanceling JobStatus = "canceling"
	JobCanceled  JobStatus = "canceled"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
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
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	Command        []string          `json:"command,omitempty"`
	Environment    map[string]string `json:"environment,omitempty"`
	Requirements   Requirements      `json:"requirements"`
	Strategy       string            `json:"strategy"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	MaxRetries     int               `json:"max_retries"`
	Attempts       int               `json:"attempts"`
	Status         JobStatus         `json:"status"`
	AssignedNode   string            `json:"assigned_node,omitempty"`
	Output         string            `json:"output,omitempty"`
	Error          string            `json:"error,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	StartedAt      *time.Time        `json:"started_at,omitempty"`
	FinishedAt     *time.Time        `json:"finished_at,omitempty"`
	RerunOf        string            `json:"rerun_of,omitempty"`
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
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Provider      string            `json:"provider"`
	Pool          string            `json:"pool"`
	GPUModel      string            `json:"gpu_model"`
	GPUCount      int               `json:"gpu_count"`
	VRAMGB        int               `json:"vram_gb"`
	HourlyPrice   float64           `json:"hourly_price"`
	Labels        map[string]string `json:"labels,omitempty"`
	Busy          bool              `json:"busy"`
	CurrentJob    string            `json:"current_job,omitempty"`
	LastHeartbeat time.Time         `json:"last_heartbeat"`
}

type JobUpdate struct {
	Status JobStatus `json:"status"`
	Output string    `json:"output,omitempty"`
	Error  string    `json:"error,omitempty"`
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
