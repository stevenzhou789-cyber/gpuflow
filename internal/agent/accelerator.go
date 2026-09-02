package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"gpuflow/internal/model"
	"gpuflow/pkg/edition"
)

const (
	backendNVIDIA = "nvidia"
	backendAscend = "ascend"
)

func (a *Agent) selectAcceleratorBackend(ctx context.Context, descriptor edition.Descriptor) error {
	requested := strings.ToLower(strings.TrimSpace(a.cfg.AcceleratorBackend))
	if requested == "" {
		requested = backendNVIDIA
	}
	if requested != backendNVIDIA && requested != backendAscend && requested != "auto" {
		return fmt.Errorf("unsupported accelerator backend %q (expected nvidia, ascend, or auto)", requested)
	}
	heterogeneous := descriptor.Features[edition.FeatureHeterogeneousAccelerators]
	if !heterogeneous {
		if requested == backendAscend {
			return errors.New("server does not enable heterogeneous accelerators; Ascend Agent registration is refused")
		}
		a.acceleratorBackend = backendNVIDIA
		return nil
	}
	if requested != "auto" {
		a.acceleratorBackend = requested
		return nil
	}

	run := a.cfg.ProbeCommand
	if run == nil {
		run = defaultProbeCommand
	}
	ascendOutput, ascendErr := run(ctx, "npu-smi", "info", "-m")
	ascendDevices, ascendParseErr := parseAscendMapping(string(ascendOutput))
	ascendAvailable := ascendErr == nil && ascendParseErr == nil && len(ascendDevices) > 0
	nvidiaOutput, nvidiaErr := run(ctx, "nvidia-smi", gpuQueryArgs...)
	_, nvidiaCount, _, nvidiaParseErr := parseGPUCapacity(string(nvidiaOutput))
	nvidiaAvailable := nvidiaErr == nil && nvidiaParseErr == nil && nvidiaCount > 0
	if ascendAvailable && nvidiaAvailable {
		return errors.New("both NVIDIA and Ascend accelerators were detected; mixed-vendor nodes are not supported in phase one")
	}
	if ascendAvailable {
		a.acceleratorBackend = backendAscend
		return nil
	}
	if nvidiaAvailable {
		a.acceleratorBackend = backendNVIDIA
		return nil
	}
	// Preserve existing CPU-node behavior when neither management tool exposes
	// an accelerator. Operators should select "ascend" explicitly when a broken
	// Ascend installation must fail closed instead of being treated as absent.
	a.acceleratorBackend = backendNVIDIA
	return nil
}

func defaultProbeCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (a *Agent) validateJobAccelerator(job *model.Job) error {
	if job.Requirements.GPUCount == 0 {
		return nil
	}
	vendor := strings.ToLower(strings.TrimSpace(job.Requirements.Labels[model.LabelAcceleratorVendor]))
	runtimeName := strings.ToLower(strings.TrimSpace(job.Requirements.Labels[model.LabelAcceleratorRuntime]))
	if vendor == "" && runtimeName == "" {
		vendor, runtimeName = model.VendorNVIDIA, model.RuntimeCUDA
	}
	wantVendor, wantRuntime := model.VendorNVIDIA, model.RuntimeCUDA
	if a.acceleratorBackend == backendAscend {
		wantVendor, wantRuntime = model.VendorHuawei, model.RuntimeCANN
	}
	if vendor != wantVendor || runtimeName != wantRuntime {
		return fmt.Errorf("job accelerator %s/%s is incompatible with node backend %s/%s", vendor, runtimeName, wantVendor, wantRuntime)
	}
	if len(job.AllocatedGPUs) > 0 {
		if len(job.AllocatedGPUs) != job.Requirements.GPUCount {
			return fmt.Errorf("job requested %d accelerator(s) but received %d allocation(s)", job.Requirements.GPUCount, len(job.AllocatedGPUs))
		}
		seen := map[int]bool{}
		for _, slot := range job.AllocatedGPUs {
			if slot < 0 || slot >= a.baseline.GPUCount || seen[slot] {
				return fmt.Errorf("invalid or duplicate accelerator slot %d", slot)
			}
			seen[slot] = true
		}
	}
	return nil
}

func (a *Agent) acceleratorDockerArgs(job *model.Job) ([]string, error) {
	if a.acceleratorBackend != backendAscend {
		return []string{"--gpus", dockerGPUSelector(job)}, nil
	}
	slots := job.AllocatedGPUs
	if len(slots) == 0 {
		slots = make([]int, job.Requirements.GPUCount)
		for index := range slots {
			slots[index] = index
		}
	}
	ids := make([]string, 0, len(slots))
	for _, slot := range slots {
		if slot < 0 || slot >= len(a.baseline.Devices) {
			return nil, fmt.Errorf("Ascend accelerator slot %d is outside the discovered inventory", slot)
		}
		id, err := ascendRuntimeID(a.baseline.Devices[slot].UUID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, strconv.Itoa(id))
	}
	return []string{"--runtime", "ascend", "-e", "ASCEND_VISIBLE_DEVICES=" + strings.Join(ids, ",")}, nil
}

func ascendRuntimeID(uuid string) (int, error) {
	const prefix = "ASCEND-"
	if !strings.HasPrefix(uuid, prefix) {
		return 0, fmt.Errorf("invalid Ascend device identity %q", uuid)
	}
	id, err := strconv.Atoi(strings.TrimPrefix(uuid, prefix))
	if err != nil || id < 0 {
		return 0, fmt.Errorf("invalid Ascend device identity %q", uuid)
	}
	return id, nil
}
