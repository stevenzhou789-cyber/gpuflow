package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"gpuflow/internal/model"
)

func TestDockerHostResourceArgs(t *testing.T) {
	args, err := dockerHostResourceArgs(model.Requirements{CPUCores: 1.5, MemoryMiB: 2048})
	want := []string{"--cpus", "1.500", "--memory", "2048m", "--memory-swap", "2048m"}
	if err != nil || !reflect.DeepEqual(args, want) {
		t.Fatalf("args %v, err %v", args, err)
	}
	args, err = dockerHostResourceArgs(model.Requirements{})
	if err != nil || len(args) != 0 {
		t.Fatal("legacy tasks must keep unset resource limits")
	}
	if _, err := dockerHostResourceArgs(model.Requirements{MemoryMiB: -1}); err == nil {
		t.Fatal("invalid limit accepted by agent")
	}
}

func TestHostCapacityUsesDockerDaemonAndNeverGuesses(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		err          error
		cpu          int
		memory       int64
		capable      bool
	}{
		{"known", "16 17179869184 true true true", nil, 8, 16384, true},
		{"daemon fewer CPUs", "4 17179869184 true true true", nil, 4, 16384, true},
		{"unavailable", "", errors.New("offline"), 0, 0, false},
		{"malformed", "16 nonsense true true true", nil, 0, 0, false},
		{"overflow", "16 99999999999999999999999999 true true true", nil, 0, 0, false},
		{"negative", "16 -1 true true true", nil, 0, 0, false},
		{"CPU quota unavailable", "16 17179869184 false true true", nil, 8, 16384, false},
		{"memory limit unavailable", "16 17179869184 true false true", nil, 8, 16384, false},
		{"swap limit unavailable", "16 17179869184 true true false", nil, 8, 16384, false},
		{"capabilities omitted", "16 17179869184", nil, 0, 0, false},
		{"malformed capability", "16 17179869184 true true unknown", nil, 8, 16384, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New(Config{CPUCores: 8, ProbeCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name != "docker" || !reflect.DeepEqual(args, []string{"info", "--format", "{{.NCPU}} {{.MemTotal}} {{.CPUCfsQuota}} {{.MemoryLimit}} {{.SwapLimit}}"}) {
					t.Fatalf("unexpected probe: %s %v", name, args)
				}
				return []byte(tc.output), tc.err
			}})
			node := model.Node{CPUCores: 999, MemoryMiB: 999, HostResourceLimits: true}
			a.probeHostCapacity(context.Background(), &node)
			if node.CPUCores != tc.cpu || node.MemoryMiB != tc.memory || node.HostResourceLimits != tc.capable {
				t.Fatalf("unexpected capacity: %+v", node)
			}
		})
	}
	a := New(Config{Executor: "mock", ProbeCommand: func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("mock invoked Docker")
		return nil, nil
	}})
	a.probeHostCapacity(context.Background(), &model.Node{})
}
