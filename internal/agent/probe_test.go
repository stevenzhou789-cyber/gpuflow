package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gpuflow/internal/model"
)

func TestProbeNodeDiscoversAggregateGPUCapacity(t *testing.T) {
	a := New(Config{CPUCores: 8, ProbeCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "docker" {
			return []byte("27.0.0"), nil
		}
		return []byte("0, NVIDIA A100, 40960\n1, NVIDIA A100, 40960\n"), nil
	}})
	node, err := a.probeNode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if node.GPUModel != "NVIDIA A100" || node.GPUCount != 2 || node.VRAMGB != 40 {
		t.Fatalf("unexpected capacity: %+v", node)
	}
}

func TestProbeNodeDiscoversPerGPUInventory(t *testing.T) {
	a := New(Config{ProbeCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "docker" {
			return []byte("27.1.0"), nil
		}
		return []byte("0, GPU-a, NVIDIA L4, 23034, 550.54\n1, GPU-b, NVIDIA L4, 23034, 550.54\n"), nil
	}})
	node, err := a.probeNode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(node.Devices) != 2 || node.Devices[1].UUID != "GPU-b" || node.DriverVersion != "550.54" || node.DockerVersion != "27.1.0" || node.HealthStatus != "HEALTHY" {
		t.Fatalf("unexpected inventory: %+v", node)
	}
}

func TestProbeNodeAllowsNativeCPUOnlyNode(t *testing.T) {
	a := New(Config{ProbeCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "docker" {
			return []byte("27.0.0"), nil
		}
		return nil, errors.New("not found")
	}})
	node, err := a.probeNode(context.Background())
	if err != nil || node.GPUModel != "none" || node.GPUCount != 0 {
		t.Fatalf("unexpected CPU-only result: node=%+v err=%v", node, err)
	}
}

func TestProbeNodeValidatesContainerRuntime(t *testing.T) {
	a := New(Config{ProbeImage: "ghcr.io/example/gpu-probe:v1", GPUProbe: "docker", ProbeCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "version" {
			return []byte("27.0.0"), nil
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--pull missing") || !strings.Contains(joined, "--gpus all") ||
			!strings.Contains(joined, "NVIDIA_VISIBLE_DEVICES=all") ||
			!strings.Contains(joined, "NVIDIA_DRIVER_CAPABILITIES=compute,utility") ||
			!strings.Contains(joined, "ghcr.io/example/gpu-probe:v1") {
			t.Fatalf("unexpected probe command: %s %s", name, joined)
		}
		return []byte("0, NVIDIA L4, 23034"), nil
	}})
	node, err := a.probeNode(context.Background())
	if err != nil || node.GPUCount != 1 || node.VRAMGB != 22 {
		t.Fatalf("unexpected container probe: node=%+v err=%v", node, err)
	}
}

func TestProbeAscendNodeDiscoversInventory(t *testing.T) {
	a := New(Config{AcceleratorBackend: "ascend", ProbeCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "docker" && strings.HasPrefix(joined, "version"):
			return []byte("27.1.0"), nil
		case name == "docker" && strings.HasPrefix(joined, "info"):
			return []byte(`{"ascend":{"path":"ascend-docker-runtime"},"runc":{"path":"runc"}}`), nil
		case name == "npu-smi" && joined == "info -m":
			return []byte("NPU ID Chip ID Chip Logic ID Chip Name\n0 0 0 Ascend 910B\n1 0 1 Ascend 910B\n"), nil
		case name == "npu-smi" && strings.Contains(joined, "-t memory"):
			return []byte("Total Capacity(MB) : 65536\n"), nil
		default:
			return nil, errors.New("unexpected command: " + name + " " + joined)
		}
	}})
	a.acceleratorBackend = backendAscend
	node, err := a.probeNode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if node.GPUModel != "Ascend 910B" || node.GPUCount != 2 || node.VRAMGB != 64 || len(node.Devices) != 2 {
		t.Fatalf("unexpected Ascend inventory: %+v", node)
	}
	if node.Labels[model.LabelAcceleratorVendor] != model.VendorHuawei || node.Labels[model.LabelAcceleratorRuntime] != model.RuntimeCANN {
		t.Fatalf("missing Ascend labels: %+v", node.Labels)
	}
}
