package agent

import (
	"context"
	"strconv"
	"strings"
	"time"

	"gpuflow/internal/model"
)

// dockerHostResourceArgs uses a single reservation/limit amount per dimension.
// Equal memory and memory-swap values disallow swap beyond the memory budget.
func dockerHostResourceArgs(r model.Requirements) ([]string, error) {
	if err := r.ValidateHostResources(); err != nil {
		return nil, err
	}
	var args []string
	if r.CPUCores > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(r.CPUCores, 'f', 3, 64))
	}
	if r.MemoryMiB > 0 {
		memory := strconv.FormatInt(r.MemoryMiB, 10) + "m"
		args = append(args, "--memory", memory, "--memory-swap", memory)
	}
	return args, nil
}

// Query the Docker daemon that will execute tasks, not the Agent container or
// workstation. Unknown capacity stays zero and cannot admit explicit requests.
func (a *Agent) probeHostCapacity(ctx context.Context, node *model.Node) {
	node.HostResourceLimits = false
	if a.cfg.Executor == "mock" {
		return
	}
	node.CPUCores, node.MemoryMiB = 0, 0
	if a.cfg.Executor != "docker" {
		return
	}
	run := a.cfg.ProbeCommand
	if run == nil {
		run = defaultProbeCommand
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := run(probeCtx, "docker", "info", "--format", "{{.NCPU}} {{.MemTotal}} {{.CPUCfsQuota}} {{.MemoryLimit}} {{.SwapLimit}}")
	if err != nil {
		return
	}
	fields := strings.Fields(string(output))
	if len(fields) != 5 {
		return
	}
	cpu, cpuErr := strconv.Atoi(fields[0])
	memoryBytes, memoryErr := strconv.ParseInt(fields[1], 10, 64)
	if cpuErr != nil || memoryErr != nil || cpu <= 0 || cpu > model.MaxCPUCores || memoryBytes <= 0 {
		return
	}
	memoryMiB := memoryBytes / (1 << 20)
	if memoryMiB < model.MinMemoryMiB || memoryMiB > model.MaxMemoryMiB {
		return
	}
	if a.cfg.CPUCores > 0 && a.cfg.CPUCores < cpu {
		cpu = a.cfg.CPUCores
	}
	node.CPUCores, node.MemoryMiB = cpu, memoryMiB
	// Capacity alone does not guarantee kernel/cgroup enforcement. Docker may
	// otherwise accept flags with a warning and silently omit a limit. Only
	// advertise the combined capability when every required control is present.
	node.HostResourceLimits = fields[2] == "true" && fields[3] == "true" && fields[4] == "true"
}
