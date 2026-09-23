package store

import "gpuflow/internal/model"

// Active reservations include assigned and canceling attempts. They are only
// released when the attempt has finished, including confirmed cancellation.
func (s *Store) reservedHostResourcesLocked(nodeID string) (float64, int64) {
	var cpu float64
	var memory int64
	for _, job := range s.state.Jobs {
		if job.AssignedNode == nodeID && activeJob(job.Status) {
			cpu += job.Requirements.CPUCores
			memory += job.Requirements.MemoryMiB
		}
	}
	return cpu, memory
}

func (s *Store) nodeHasHostCapacityLocked(job *model.Job, node *model.Node) bool {
	r := job.Requirements
	if r.ValidateHostResources() != nil || ((r.CPUCores > 0 || r.MemoryMiB > 0) && !node.HostResourceLimits) {
		return false
	}
	cpu, memory := s.reservedHostResourcesLocked(node.ID)
	// An unknown (zero) capacity cannot satisfy a positive request. Unset
	// dimensions intentionally retain the old unreserved behavior.
	return (r.CPUCores == 0 || r.CPUCores <= float64(node.CPUCores)-cpu+1e-9) &&
		(r.MemoryMiB == 0 || r.MemoryMiB <= node.MemoryMiB-memory)
}
