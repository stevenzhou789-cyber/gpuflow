package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuflow/internal/model"
	"gpuflow/pkg/projectscope"
)

func maintenanceNode(t *testing.T, s *Store, id string) *model.Node {
	t.Helper()
	node, err := s.RegisterNodeSession(model.Node{ID: id, GPUCount: 2, GPUModel: "L4", VRAMGB: 24}, "session")
	if err != nil {
		t.Fatal(err)
	}
	confirmAgentSession(t, s, id, "session")
	return node
}

func maintenanceJob(t *testing.T, s *Store, name string) *model.Job {
	t.Helper()
	job, err := s.CreateJob(model.JobCreate{Name: name, Image: "work", Requirements: model.Requirements{GPUCount: 1}, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestNodeMaintenancePreservesExistingAttemptsAndStopsNewAssignments(t *testing.T) {
	for _, fair := range []bool{false, true} {
		for _, phase := range []string{"assigned", "claimed", "running", "canceling"} {
			t.Run(fmt.Sprintf("fair=%t/%s", fair, phase), func(t *testing.T) {
				s := NewMemory()
				s.SetProjectFairScheduling(fair)
				s.SetGPUGranularScheduling(fair)
				node := maintenanceNode(t, s, "worker")
				job := maintenanceJob(t, s, "existing")
				if err := s.Schedule(time.Minute); err != nil {
					t.Fatal(err)
				}
				var dispatch *model.AgentJob
				var err error
				if phase != "assigned" {
					dispatch, err = s.NextJobSession(node.ID, "session")
					if err != nil || dispatch == nil {
						t.Fatalf("claim: %+v %v", dispatch, err)
					}
				}
				if phase == "running" || phase == "canceling" {
					if _, err := s.UpdateJobLease(job.ID, node.ID, "session", dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "canceling" {
					if _, err := s.CancelJob(job.ID); err != nil {
						t.Fatal(err)
					}
				}
				before, _ := s.GetJob(job.ID)
				maintenance, err := s.SetNodeMaintenance(node.ID, true)
				if err != nil || !maintenance.Maintenance || maintenance.MaintenanceState != model.NodeMaintenanceDraining || maintenance.MaintenanceUpdatedAt == nil {
					t.Fatalf("maintenance: %+v %v", maintenance, err)
				}
				after, _ := s.GetJob(job.ID)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("enabling maintenance changed an existing attempt")
				}
				waiting := maintenanceJob(t, s, "waiting")
				if err := s.Schedule(time.Minute); err != nil {
					t.Fatal(err)
				}
				queued, _ := s.GetJob(waiting.ID)
				if queued.Status != model.JobQueued {
					t.Fatalf("new work was assigned during maintenance: %+v", queued)
				}
				explanation, err := s.GetJobSchedulingForScope(projectscope.All(), waiting.ID, time.Minute)
				if err != nil || explanation.ReasonCode != SchedulingReasonNodesInMaintenance || explanation.Nodes.Maintenance != 1 || explanation.Nodes.Ready != 0 {
					t.Fatalf("maintenance explanation: %+v %v", explanation, err)
				}
				if dispatch == nil {
					dispatch, err = s.NextJobSession(node.ID, "session")
					if err != nil || dispatch == nil || dispatch.ID != job.ID {
						t.Fatalf("maintenance blocked an existing assignment: %+v %v", dispatch, err)
					}
				}
				if phase == "assigned" || phase == "claimed" {
					if _, err := s.UpdateJobLease(job.ID, node.ID, "session", dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
						t.Fatalf("maintenance blocked starting a claimed attempt: %v", err)
					}
				}
				if err := s.HeartbeatNodeSession(node.ID, "session"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.UpdateJobOutputLease(job.ID, node.ID, "session", dispatch.AttemptToken, "finishing"); err != nil {
					t.Fatal(err)
				}
				if err := s.ValidateJobAttempt(job.ID, node.ID, "session", dispatch.AttemptToken); err != nil {
					t.Fatal(err)
				}
				terminal := model.JobSucceeded
				if phase == "canceling" {
					terminal = model.JobCanceled
				}
				if _, err := s.UpdateJobLease(job.ID, node.ID, "session", dispatch.AttemptToken, model.JobUpdate{Status: terminal}); err != nil {
					t.Fatal(err)
				}
				if drained := s.ListNodes()[0]; drained.MaintenanceState != model.NodeMaintenanceDrained || drained.Busy || !drained.Maintenance {
					t.Fatalf("completed attempts did not drain: %+v", drained)
				}
				if err := s.Schedule(time.Minute); err != nil {
					t.Fatal(err)
				}
				queued, _ = s.GetJob(waiting.ID)
				if queued.Status != model.JobQueued {
					t.Fatal("drained node automatically resumed scheduling")
				}
				resumed, err := s.SetNodeMaintenance(node.ID, false)
				if err != nil || resumed.Maintenance || resumed.MaintenanceState != model.NodeMaintenanceActive || resumed.MaintenanceUpdatedAt.Before(*maintenance.MaintenanceUpdatedAt) {
					t.Fatalf("explicit resume: %+v %v", resumed, err)
				}
				if err := s.Schedule(time.Minute); err != nil {
					t.Fatal(err)
				}
				assigned, _ := s.GetJob(waiting.ID)
				if assigned.Status != model.JobAssigned {
					t.Fatal("explicit resume did not restore scheduling")
				}
			})
		}
	}
}

func TestNodeMaintenanceWaitsForCleanupAndSurvivesAgentRestart(t *testing.T) {
	s := NewMemory()
	node := maintenanceNode(t, s, "restart")
	job := maintenanceJob(t, s, "interrupted")
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	dispatch, err := s.NextJobSession(node.ID, "session")
	if err != nil || dispatch == nil {
		t.Fatalf("claim: %+v %v", dispatch, err)
	}
	if _, err := s.UpdateJobLease(job.ID, node.ID, "session", dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatal(err)
	}
	maintenance, err := s.SetNodeMaintenance(node.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	setAt := *maintenance.MaintenanceUpdatedAt
	// Repeated management calls and caller mutation cannot replace the saved timestamp.
	maintenance.MaintenanceUpdatedAt = nil
	maintenance, err = s.SetNodeMaintenance(node.ID, true)
	if err != nil || !maintenance.MaintenanceUpdatedAt.Equal(setAt) {
		t.Fatal("idempotent maintenance changed its timestamp")
	}
	*maintenance.MaintenanceUpdatedAt = time.Time{}
	s.mu.Lock()
	s.state.Nodes[node.ID].LastHeartbeat = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	forged := time.Now().Add(time.Hour)
	restarted, err := s.RegisterNodeSession(model.Node{ID: node.ID, GPUCount: 2, GPUModel: "L4", VRAMGB: 24, Maintenance: false, MaintenanceState: model.NodeMaintenanceDrained, MaintenanceUpdatedAt: &forged}, "replacement")
	if err != nil || !restarted.Maintenance || restarted.MaintenanceState != model.NodeMaintenanceDraining || !restarted.CleanupPending || !restarted.MaintenanceUpdatedAt.Equal(setAt) {
		t.Fatalf("registration overwrote maintenance: %+v %v", restarted, err)
	}
	if err := s.DeleteNode(node.ID); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("deleted uncleared node: %v", err)
	}
	confirmed, err := s.ConfirmNodeCleanupSession(node.ID, "replacement")
	if err != nil || confirmed.MaintenanceState != model.NodeMaintenanceDraining {
		t.Fatalf("cleanup alone released an active recovery attempt: %+v %v", confirmed, err)
	}
	cleanup, err := s.NextJobSession(node.ID, "replacement")
	if err != nil || cleanup == nil || cleanup.Status != model.JobCanceling {
		t.Fatalf("maintenance blocked recovery cleanup: %+v %v", cleanup, err)
	}
	if _, err := s.UpdateJobLease(job.ID, node.ID, "replacement", cleanup.AttemptToken, model.JobUpdate{Status: model.JobCanceled}); err != nil {
		t.Fatal(err)
	}
	healthy, err := s.UpdateNodeHealthSession(node.ID, "replacement", model.NodeHealthUpdate{Status: "HEALTHY", GPUCount: 2, GPUModel: "L4", VRAMGB: 24})
	if err != nil || !healthy.Maintenance || healthy.MaintenanceState != model.NodeMaintenanceDrained || !healthy.MaintenanceUpdatedAt.Equal(setAt) {
		t.Fatalf("health recovery changed maintenance: %+v %v", healthy, err)
	}
	if err := s.HeartbeatNodeSession(node.ID, "replacement"); err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	queued, _ := s.GetJob(job.ID)
	if queued.Status != model.JobQueued || !s.ListNodes()[0].Maintenance {
		t.Fatalf("retry escaped maintenance: %+v", queued)
	}
}

func TestNodeMaintenanceCleanupPendingWithoutJobsIsNotDrained(t *testing.T) {
	s := NewMemory()
	forged := time.Now()
	node, err := s.RegisterNodeSession(model.Node{ID: "empty", Maintenance: true, MaintenanceState: model.NodeMaintenanceDrained, MaintenanceUpdatedAt: &forged}, "session")
	if err != nil || node.Maintenance || node.MaintenanceUpdatedAt != nil || node.MaintenanceState != model.NodeMaintenanceActive {
		t.Fatalf("Agent controlled maintenance on registration: %+v %v", node, err)
	}
	maintenance, err := s.SetNodeMaintenance(node.ID, true)
	if err != nil || maintenance.Busy || maintenance.MaintenanceState != model.NodeMaintenanceDraining {
		t.Fatalf("cleanup-pending node looked drained: %+v %v", maintenance, err)
	}
	if err := s.DeleteNode(node.ID); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("deleted pending cleanup node: %v", err)
	}
	ready, err := s.ConfirmNodeCleanupSession(node.ID, "session")
	if err != nil || ready.MaintenanceState != model.NodeMaintenanceDrained {
		t.Fatalf("clean empty node did not drain: %+v %v", ready, err)
	}
	if err := s.DeleteNode(node.ID); err != nil {
		t.Fatalf("cannot explicitly delete drained node: %v", err)
	}
}

func TestNodeMaintenanceAndSchedulingSerialize(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		s := NewMemory()
		s.SetGPUGranularScheduling(true)
		maintenanceNode(t, s, "racing")
		for i := 0; i < 3; i++ {
			maintenanceJob(t, s, fmt.Sprint(i))
		}
		start := make(chan struct{})
		scheduled := make(chan error, 1)
		type maintenanceResult struct {
			node *model.Node
			err  error
		}
		changed := make(chan maintenanceResult, 1)
		go func() { <-start; scheduled <- s.Schedule(time.Minute) }()
		go func() {
			<-start
			node, err := s.SetNodeMaintenance("racing", true)
			changed <- maintenanceResult{node, err}
		}()
		close(start)
		result := <-changed
		if result.err != nil {
			t.Fatal(result.err)
		}
		if err := <-scheduled; err != nil {
			t.Fatal(err)
		}
		// Schedule may win the lock and reserve work first. It may never add
		// assignments after the maintenance response's atomic snapshot.
		if final := s.ListNodes()[0]; !reflect.DeepEqual(final.ActiveJobs, result.node.ActiveJobs) {
			t.Fatalf("assignment crossed maintenance boundary: before=%v after=%v", result.node.ActiveJobs, final.ActiveJobs)
		}
	}
}

func TestNodeMaintenanceDetailsRoundTripAndRollback(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, enabled := range []bool{false, true} {
		original := model.Node{Maintenance: enabled, MaintenanceState: model.NodeMaintenanceDraining, MaintenanceUpdatedAt: &now, SessionEpoch: "session", CleanupPending: true}
		payload, err := json.Marshal(detailsFromNode(&original))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), "maintenance_state") {
			t.Fatal("derived drain state was persisted")
		}
		var details nodeDetails
		if err := json.Unmarshal(payload, &details); err != nil {
			t.Fatal(err)
		}
		var restored model.Node
		details.apply(&restored)
		if restored.Maintenance != enabled || restored.MaintenanceUpdatedAt == nil || !restored.MaintenanceUpdatedAt.Equal(now) || !restored.CleanupPending {
			t.Fatalf("lost durable maintenance state: %+v", restored)
		}
	}
	var legacy nodeDetails
	if err := json.Unmarshal([]byte(`{"session_epoch":"session","cleanup_pending":false}`), &legacy); err != nil {
		t.Fatal(err)
	}
	var restored model.Node
	legacy.apply(&restored)
	if restored.Maintenance || restored.MaintenanceUpdatedAt != nil {
		t.Fatal("legacy node unexpectedly entered maintenance")
	}
	s := NewMemory()
	maintenanceNode(t, s, "rollback")
	db, err := sql.Open("mysql", "unused@tcp(127.0.0.1:1)/unused")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s.db = db
	if _, err := s.SetNodeMaintenance("rollback", true); !errors.Is(err, ErrPersistence) {
		t.Fatalf("missing persistence error: %v", err)
	}
	s.db = nil
	if node := s.ListNodes()[0]; node.Maintenance || node.MaintenanceUpdatedAt != nil || node.MaintenanceState != model.NodeMaintenanceActive {
		t.Fatalf("failed maintenance change was not rolled back: %+v", node)
	}
	if _, err := s.SetNodeMaintenance("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
}
