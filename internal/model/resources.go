package model

import (
	"fmt"
	"math"
)

const (
	MaxCPUCores        = 65536
	MinCPUCores        = 0.01 // Default Linux CFS period requires at least 1 ms quota.
	MaxMemoryMiB int64 = 1 << 30
	MinMemoryMiB int64 = 6 // Docker's minimum memory limit.
)

// ValidateHostResources bounds conversions to Docker quota/byte values. CPU
// requests have millicore precision so accounting and enforced quotas agree.
func (r Requirements) ValidateHostResources() error {
	if math.IsNaN(r.CPUCores) || math.IsInf(r.CPUCores, 0) || r.CPUCores < 0 || r.CPUCores > MaxCPUCores {
		return fmt.Errorf("cpu_cores must be finite and between 0 and %d", MaxCPUCores)
	}
	if r.CPUCores > 0 && (r.CPUCores < MinCPUCores || math.Abs(r.CPUCores*1000-math.Round(r.CPUCores*1000)) > 0.000001) {
		return fmt.Errorf("cpu_cores must be at least %.2f and use increments of 0.001", MinCPUCores)
	}
	if r.MemoryMiB < 0 || r.MemoryMiB > MaxMemoryMiB || (r.MemoryMiB > 0 && r.MemoryMiB < MinMemoryMiB) {
		return fmt.Errorf("memory_mib must be 0 or between %d and %d", MinMemoryMiB, MaxMemoryMiB)
	}
	return nil
}
