package api

import (
	"gpuflow/internal/model"
	"gpuflow/pkg/edition"
)

// The HTTP product boundary is enforced by the control plane, not by trusting
// an Agent to strip fields. Basic capacity and health reports remain supported.
func (s *Server) acceptsNodeInventory(devices []model.GPUDevice, driver, docker string) bool {
	return s.edition.Features[edition.FeaturePerGPUInventory] ||
		(len(devices) == 0 && driver == "" && docker == "")
}

// Project a copy so reads do not erase inventory retained in persisted state
// or used by another distribution. Enterprise receives the complete model.
func (s *Server) projectNode(node *model.Node) *model.Node {
	if node == nil || s.edition.Features[edition.FeaturePerGPUInventory] {
		return node
	}
	projected := *node
	projected.Devices, projected.DriverVersion, projected.DockerVersion = nil, "", ""
	return &projected
}

func (s *Server) projectNodes(nodes []*model.Node) []*model.Node {
	for i, node := range nodes {
		nodes[i] = s.projectNode(node)
	}
	return nodes
}
