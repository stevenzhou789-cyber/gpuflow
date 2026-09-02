package agent

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuflow/internal/model"
)

type ascendMapping struct {
	NPU      int
	Chip     int
	Model    string
	MemoryMB int
}

func (a *Agent) probeAscendNode(ctx context.Context) (model.Node, error) {
	node := model.Node{
		ID: a.cfg.ID, Name: a.cfg.Name, Provider: a.cfg.Provider, Pool: a.cfg.Pool,
		GPUModel: "none", CPUCores: a.cfg.CPUCores, HourlyPrice: a.cfg.HourlyPrice,
		Labels: map[string]string{
			model.LabelAcceleratorVendor:  model.VendorHuawei,
			model.LabelAcceleratorRuntime: model.RuntimeCANN,
		},
	}
	now := time.Now().UTC()
	node.HealthStatus, node.LastHealthCheck = "HEALTHY", &now
	if a.cfg.Executor == "mock" {
		return node, nil
	}
	run := a.cfg.ProbeCommand
	if run == nil {
		run = defaultProbeCommand
	}
	dockerVersion, err := run(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return node, fmt.Errorf("Docker is unavailable: %s: %w", strings.TrimSpace(string(dockerVersion)), err)
	}
	node.DockerVersion = strings.TrimSpace(string(dockerVersion))
	runtimes, err := run(ctx, "docker", "info", "--format", "{{json .Runtimes}}")
	if err != nil || !strings.Contains(strings.ToLower(string(runtimes)), "ascend") {
		return node, fmt.Errorf("Ascend Docker Runtime is unavailable: %s: %w", strings.TrimSpace(string(runtimes)), errOrUnavailable(err))
	}
	mappingOutput, err := run(ctx, "npu-smi", "info", "-m")
	if err != nil {
		return node, fmt.Errorf("npu-smi inventory failed: %s: %w", strings.TrimSpace(string(mappingOutput)), err)
	}
	mappings, err := parseAscendMapping(string(mappingOutput))
	if err != nil {
		return node, err
	}
	if len(mappings) == 0 {
		return node, fmt.Errorf("npu-smi reported no Ascend devices")
	}
	for index := range mappings {
		memoryOutput, memoryErr := run(ctx, "npu-smi", "info", "-t", "memory", "-i", strconv.Itoa(mappings[index].NPU))
		if memoryErr != nil {
			return node, fmt.Errorf("query Ascend device %d memory: %s: %w", mappings[index].NPU, strings.TrimSpace(string(memoryOutput)), memoryErr)
		}
		memoryMB, memoryErr := parseAscendMemory(string(memoryOutput))
		if memoryErr != nil {
			return node, fmt.Errorf("query Ascend device %d memory: %w", mappings[index].NPU, memoryErr)
		}
		mappings[index].MemoryMB = memoryMB
	}
	applyAscendInventory(&node, mappings)
	return node, nil
}

func parseAscendMapping(output string) ([]ascendMapping, error) {
	byNPU := map[int]ascendMapping{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 4 {
			continue
		}
		npuID, npuErr := strconv.Atoi(fields[0])
		chipID, chipErr := strconv.Atoi(fields[1])
		if npuErr != nil || chipErr != nil || npuID < 0 || strings.EqualFold(fields[len(fields)-1], "mcu") {
			continue
		}
		modelName := strings.Join(fields[3:], " ")
		if modelName == "" {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(modelName), "ascend") {
			modelName = "Ascend " + modelName
		}
		if _, exists := byNPU[npuID]; !exists {
			byNPU[npuID] = ascendMapping{NPU: npuID, Chip: chipID, Model: modelName}
		}
	}
	result := make([]ascendMapping, 0, len(byNPU))
	for _, device := range byNPU {
		result = append(result, device)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].NPU < result[j].NPU })
	return result, nil
}

func parseAscendMemory(output string) (int, error) {
	for _, line := range strings.Split(output, "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		if key != "total capacity(mb)" && key != "hbm capacity(mb)" {
			continue
		}
		value := strings.Fields(strings.TrimSpace(parts[1]))
		if len(value) == 0 {
			continue
		}
		memoryMB, err := strconv.Atoi(value[0])
		if err == nil && memoryMB > 0 {
			return memoryMB, nil
		}
	}
	return 0, fmt.Errorf("npu-smi output does not contain positive device memory")
}

func applyAscendInventory(node *model.Node, mappings []ascendMapping) {
	node.Devices = make([]model.GPUDevice, 0, len(mappings))
	minimumMB := 0
	modelName := ""
	for index, device := range mappings {
		if modelName == "" {
			modelName = device.Model
		} else if modelName != device.Model {
			modelName = "mixed"
		}
		if minimumMB == 0 || device.MemoryMB < minimumMB {
			minimumMB = device.MemoryMB
		}
		node.Devices = append(node.Devices, model.GPUDevice{
			Index: index, UUID: "ASCEND-" + strconv.Itoa(device.NPU), Model: device.Model,
			VRAMGB: (device.MemoryMB + 512) / 1024,
		})
	}
	node.GPUModel, node.GPUCount = modelName, len(mappings)
	node.VRAMGB = (minimumMB + 512) / 1024
}

func errOrUnavailable(err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("runtime is not registered with Docker")
}
