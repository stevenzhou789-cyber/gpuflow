package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"gpuflow/internal/model"
)

var gpuQueryArgs = []string{"--query-gpu=index,name,memory.total", "--format=csv,noheader,nounits"}

func (a *Agent) probeNode(ctx context.Context) (model.Node, error) {
	node := model.Node{
		ID: a.cfg.ID, Name: a.cfg.Name, Provider: a.cfg.Provider, Pool: a.cfg.Pool,
		GPUModel: "none", CPUCores: a.cfg.CPUCores, HourlyPrice: a.cfg.HourlyPrice,
	}
	if a.cfg.Executor == "mock" {
		return node, nil
	}
	run := a.cfg.ProbeCommand
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	if output, err := run(ctx, "docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		return node, fmt.Errorf("Docker is unavailable: %s: %w", strings.TrimSpace(string(output)), err)
	}
	var output []byte
	var err error
	if image := strings.TrimSpace(a.cfg.ProbeImage); image != "" {
		args := []string{"run", "--rm", "--pull", "never", "--gpus", "all", "--entrypoint", "nvidia-smi", image}
		output, err = run(ctx, "docker", append(args, gpuQueryArgs...)...)
		if err != nil {
			return node, fmt.Errorf("NVIDIA container runtime validation failed: %s: %w", strings.TrimSpace(string(output)), err)
		}
	} else {
		output, err = run(ctx, "nvidia-smi", gpuQueryArgs...)
		if err != nil {
			return node, nil
		}
	}
	modelName, count, vram, err := parseGPUCapacity(string(output))
	if err != nil {
		return node, err
	}
	node.GPUModel, node.GPUCount, node.VRAMGB = modelName, count, vram
	return node, nil
}

func parseGPUCapacity(output string) (string, int, int, error) {
	var modelName string
	count, minimumMiB := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 3 {
			return "", 0, 0, fmt.Errorf("unexpected nvidia-smi output: %q", line)
		}
		name := strings.TrimSpace(fields[1])
		memoryMiB, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil || name == "" || memoryMiB <= 0 {
			return "", 0, 0, fmt.Errorf("unexpected nvidia-smi output: %q", line)
		}
		if count == 0 {
			modelName, minimumMiB = name, memoryMiB
		} else {
			if modelName != name {
				modelName = "mixed"
			}
			if memoryMiB < minimumMiB {
				minimumMiB = memoryMiB
			}
		}
		count++
	}
	if count == 0 {
		return "none", 0, 0, nil
	}
	return modelName, count, (minimumMiB + 512) / 1024, nil
}
