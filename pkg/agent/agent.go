// Package agent exposes the supported Agent composition API for packaged
// distributions while keeping the executor implementation in the core.
package agent

import (
	"context"
	"time"

	core "gpuflow/internal/agent"
)

type Config struct {
	Server, Token, ID, Name, Provider, Pool, GPUModel, Executor, ArtifactDir string
	GPUCount, CPUCores, VRAMGB                                               int
	HourlyPrice                                                              float64
	PollInterval, HeartbeatInterval, ArtifactUploadTimeout                   time.Duration
}

func Run(ctx context.Context, cfg Config) error {
	return core.New(core.Config{
		Server: cfg.Server, Token: cfg.Token, ID: cfg.ID, Name: cfg.Name,
		Provider: cfg.Provider, Pool: cfg.Pool, GPUModel: cfg.GPUModel,
		Executor: cfg.Executor, ArtifactDir: cfg.ArtifactDir,
		GPUCount: cfg.GPUCount, CPUCores: cfg.CPUCores, VRAMGB: cfg.VRAMGB, HourlyPrice: cfg.HourlyPrice,
		PollInterval: cfg.PollInterval, HeartbeatInterval: cfg.HeartbeatInterval,
		ArtifactUploadTimeout: cfg.ArtifactUploadTimeout,
	}).Run(ctx)
}
