package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"gpuflow/internal/model"
)

var gpuQueryArgs = []string{"--query-gpu=index,uuid,name,memory.total,driver_version", "--format=csv,noheader,nounits"}

func (a *Agent) probeNode(ctx context.Context) (model.Node, error) {
	if a.acceleratorBackend == "ascend" {
		return a.probeAscendNode(ctx)
	}
	node := model.Node{
		ID: a.cfg.ID, Name: a.cfg.Name, Provider: a.cfg.Provider, Pool: a.cfg.Pool,
		GPUModel: "none", CPUCores: a.cfg.CPUCores, HourlyPrice: a.cfg.HourlyPrice,
	}
	now := time.Now().UTC()
	node.HealthStatus, node.LastHealthCheck = "HEALTHY", &now
	if a.cfg.Executor == "mock" {
		return node, nil
	}
	mode := strings.ToLower(strings.TrimSpace(a.cfg.GPUProbe))
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" && mode != "host" && mode != "docker" {
		return node, fmt.Errorf("unsupported GPU probe mode %q (expected auto, host, or docker)", a.cfg.GPUProbe)
	}
	run := a.cfg.ProbeCommand
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	var dockerErr error
	if a.cfg.Executor == "docker" || mode == "docker" {
		if output, err := run(ctx, "docker", "version", "--format", "{{.Server.Version}}"); err != nil {
			dockerErr = fmt.Errorf("Docker is unavailable: %s: %w", strings.TrimSpace(string(output)), err)
		} else {
			node.DockerVersion = strings.TrimSpace(string(output))
		}
	}

	hostInventory := false
	if mode != "docker" {
		output, err := run(ctx, "nvidia-smi", gpuQueryArgs...)
		if err == nil {
			if err := applyGPUInventory(&node, output); err != nil {
				return node, err
			}
			hostInventory = node.GPUCount > 0
		} else if mode == "host" && dockerErr != nil {
			return node, dockerErr
		}
	}

	image := strings.TrimSpace(a.cfg.ProbeImage)
	useDockerProbe := mode == "docker" || (mode == "auto" && image != "")
	if !useDockerProbe {
		if dockerErr != nil {
			return node, dockerErr
		}
		return node, nil
	}
	if image == "" {
		return node, errors.New("docker GPU probe requires a dedicated probe image")
	}
	if dockerErr != nil {
		return node, dockerErr
	}
	{
		// Enterprise Agents authenticate to the control-plane Registry before
		// entering this shared runtime; Community receives an immutable public
		// image reference. Pull only when missing so preloaded offline nodes stay
		// independent of an external Registry.
		args := []string{
			"run", "--rm", "--pull", "missing", "--gpus", "all",
			"-e", "NVIDIA_VISIBLE_DEVICES=all",
			"-e", "NVIDIA_DRIVER_CAPABILITIES=compute,utility",
			"--entrypoint", "nvidia-smi", image,
		}
		output, err := run(ctx, "docker", append(args, gpuQueryArgs...)...)
		if err != nil {
			if hostInventory {
				return node, fmt.Errorf("NVIDIA container runtime validation failed after host GPU discovery: %s: %w", strings.TrimSpace(string(output)), err)
			}
			return node, fmt.Errorf("NVIDIA container runtime validation failed: %s: %w", strings.TrimSpace(string(output)), err)
		}
		if err := applyGPUInventory(&node, output); err != nil {
			return node, err
		}
	}
	return node, nil
}

func applyGPUInventory(node *model.Node, output []byte) error {
	modelName, count, vram, devices, driverVersion, err := parseGPUInventory(string(output))
	if err != nil {
		return err
	}
	node.GPUModel, node.GPUCount, node.VRAMGB = modelName, count, vram
	node.Devices, node.DriverVersion = devices, driverVersion
	return nil
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
