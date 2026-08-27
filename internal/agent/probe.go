package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"gpuflow/internal/model"
)

var gpuQueryArgs = []string{"--query-gpu=index,uuid,name,memory.total,driver_version", "--format=csv,noheader,nounits"}

func (a *Agent) probeNode(ctx context.Context) (model.Node, error) {
	node := model.Node{
		ID: a.cfg.ID, Name: a.cfg.Name, Provider: a.cfg.Provider, Pool: a.cfg.Pool,
		GPUModel: "none", CPUCores: a.cfg.CPUCores, HourlyPrice: a.cfg.HourlyPrice,
	}
	now := time.Now().UTC()
	node.HealthStatus, node.LastHealthCheck = "HEALTHY", &now
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
	} else {
		node.DockerVersion = strings.TrimSpace(string(output))
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
	modelName, count, vram, devices, driverVersion, err := parseGPUInventory(string(output))
	if err != nil {
		return node, err
	}
	node.GPUModel, node.GPUCount, node.VRAMGB = modelName, count, vram
	node.Devices, node.DriverVersion = devices, driverVersion
	return node, nil
}

func parseGPUCapacity(output string) (string, int, int, error) {
	modelName, count, vram, _, _, err := parseGPUInventory(output)
	return modelName, count, vram, err
}

func parseGPUInventory(output string) (string, int, int, []model.GPUDevice, string, error) {
	var modelName string
	var driverVersion string
	devices := []model.GPUDevice{}
	count, minimumMiB := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 3 && len(fields) != 5 {
			return "", 0, 0, nil, "", fmt.Errorf("unexpected nvidia-smi output: %q", line)
		}
		indexField, uuid, nameField, memoryField, driverField := fields[0], "", fields[1], fields[2], ""
		if len(fields) == 5 {
			indexField, uuid, nameField, memoryField, driverField = fields[0], strings.TrimSpace(fields[1]), fields[2], fields[3], fields[4]
		}
		index, indexErr := strconv.Atoi(strings.TrimSpace(indexField))
		name := strings.TrimSpace(nameField)
		memoryMiB, err := strconv.Atoi(strings.TrimSpace(memoryField))
		if indexErr != nil || err != nil || name == "" || memoryMiB <= 0 {
			return "", 0, 0, nil, "", fmt.Errorf("unexpected nvidia-smi output: %q", line)
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
		currentDriver := strings.TrimSpace(driverField)
		if driverVersion == "" {
			driverVersion = currentDriver
		} else if currentDriver != "" && driverVersion != currentDriver {
			driverVersion = "mixed"
		}
		devices = append(devices, model.GPUDevice{Index: index, UUID: uuid, Model: name, VRAMGB: (memoryMiB + 512) / 1024})
		count++
	}
	if count == 0 {
		return "none", 0, 0, devices, driverVersion, nil
	}
	return modelName, count, (minimumMiB + 512) / 1024, devices, driverVersion, nil
}
