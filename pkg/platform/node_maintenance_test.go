package platform

import (
	"errors"
	"testing"

	"gpuflow/internal/model"
	"gpuflow/internal/store"
)

func TestRuntimeProvidesNodeMaintenanceController(t *testing.T) {
	state := store.NewMemory()
	if _, err := state.RegisterNode(model.Node{ID: "maintenance"}); err != nil {
		t.Fatal(err)
	}
	var controller NodeMaintenanceController = &Runtime{state: state}
	node, err := controller.SetNodeMaintenance("maintenance", true)
	if err != nil || !node.Maintenance || node.MaintenanceState != NodeMaintenanceDrained || node.MaintenanceUpdatedAt == nil {
		t.Fatalf("maintenance controller: %+v %v", node, err)
	}
	node, err = controller.SetNodeMaintenance("maintenance", false)
	if err != nil || node.Maintenance || node.MaintenanceState != NodeMaintenanceActive {
		t.Fatalf("resume controller: %+v %v", node, err)
	}
	if _, err := controller.SetNodeMaintenance("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
}
