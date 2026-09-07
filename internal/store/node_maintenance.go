package store

import (
	"time"

	"gpuflow/internal/model"
)

// SetNodeMaintenance changes only admission of new assignments. Already
// assigned or running attempts retain their leases and finish normally.
// Sharing Store.mu with Schedule makes the change atomic with assignment.
func (s *Store) SetNodeMaintenance(id string, enabled bool) (*model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.state.Nodes[id]
	if node == nil {
		return nil, ErrNotFound
	}
	if node.Maintenance != enabled {
		before := cloneSnapshot(s.state)
		now := time.Now().UTC()
		node.Maintenance, node.MaintenanceUpdatedAt = enabled, &now
		if err := s.commitLocked(before); err != nil {
			return nil, err
		}
	}
	return s.cloneNodeLocked(node), nil
}

func (s *Store) nodeMaintenanceStateLocked(node *model.Node) string {
	if !node.Maintenance {
		return model.NodeMaintenanceActive
	}
	if node.CleanupPending {
		return model.NodeMaintenanceDraining
	}
	for _, job := range s.state.Jobs {
		if job.AssignedNode == node.ID && activeJob(job.Status) {
			return model.NodeMaintenanceDraining
		}
	}
	return model.NodeMaintenanceDrained
}

// Management state is derived on read, not trusted from Agent payloads or
// persisted as a potentially stale drain-completion snapshot.
func (s *Store) cloneNodeLocked(node *model.Node) *model.Node {
	copy := *node
	copy.Labels = cloneStringMap(node.Labels)
	copy.ActiveJobs = append([]string(nil), node.ActiveJobs...)
	copy.Devices = append([]model.GPUDevice(nil), node.Devices...)
	copy.LastHealthCheck = cloneTime(node.LastHealthCheck)
	copy.MaintenanceUpdatedAt = cloneTime(node.MaintenanceUpdatedAt)
	copy.MaintenanceState = s.nodeMaintenanceStateLocked(node)
	return &copy
}
