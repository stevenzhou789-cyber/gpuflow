package store

import (
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuflow/internal/model"
)

func confirmAgentSession(t *testing.T, s *Store, nodeID, session string) {
	t.Helper()
	if _, err := s.ConfirmNodeCleanupSession(nodeID, session); err != nil {
		t.Fatalf("confirm node cleanup for %s: %v", nodeID, err)
	}
}

func TestScheduleChoosesCheapestEligibleNode(t *testing.T) {
	s := NewMemory()
	_, _ = s.RegisterNode(model.Node{ID: "expensive", Name: "expensive", GPUModel: "RTX 4090", GPUCount: 1, VRAMGB: 24, HourlyPrice: 4})
	_, _ = s.RegisterNode(model.Node{ID: "cheap", Name: "cheap", GPUModel: "RTX 4090", GPUCount: 1, VRAMGB: 24, HourlyPrice: 2})
	j, err := s.CreateJob(model.JobCreate{Name: "test", Image: "alpine", Requirements: model.Requirements{GPUCount: 1, MinVRAMGB: 20}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(j.ID)
	if got.AssignedNode != "cheap" {
		t.Fatalf("expected cheap node, got %q", got.AssignedNode)
	}
}

func TestAgentSessionCleanupGateBlocksDispatchUntilConfirmed(t *testing.T) {
	s := NewMemory()
	node, err := s.RegisterNodeSession(model.Node{ID: "cleanup-gated", GPUCount: 1, VRAMGB: 24}, "session-one")
	if err != nil {
		t.Fatal(err)
	}
	if !node.CleanupPending {
		t.Fatal("new Agent session was schedulable before orphan cleanup")
	}
	job, err := s.CreateJob(model.JobCreate{Name: "wait for cleanup", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.GetJob(job.ID)
	if stored.Status != model.JobQueued {
		t.Fatalf("pending cleanup session received work: %+v", stored)
	}
	if err := s.HeartbeatNodeSession(node.ID, "session-one"); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("pending session bypassed cleanup through heartbeat: %v", err)
	}
	if _, err := s.NextJobSession(node.ID, "session-one"); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("pending session polled work before cleanup: %v", err)
	}
	if _, err := s.ConfirmNodeCleanupSession(node.ID, "wrong-session"); !errors.Is(err, ErrAgentSession) {
		t.Fatalf("wrong session confirmed cleanup: %v", err)
	}
	ready, err := s.ConfirmNodeCleanupSession(node.ID, "session-one")
	if err != nil || ready.CleanupPending {
		t.Fatalf("current session could not confirm cleanup: %+v %v", ready, err)
	}
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	stored, _ = s.GetJob(job.ID)
	if stored.Status != model.JobAssigned || stored.AssignedSession != "session-one" {
		t.Fatalf("ready session did not receive work: %+v", stored)
	}

	s.mu.Lock()
	s.state.Nodes[node.ID].LastHeartbeat = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	if _, err := s.RegisterNodeSession(model.Node{ID: node.ID, GPUCount: 1, VRAMGB: 24}, "session-two"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmNodeCleanupSession(node.ID, "session-one"); !errors.Is(err, ErrAgentSession) {
		t.Fatalf("old session confirmed a new takeover: %v", err)
	}
}

func TestExpiredAgentSessionIsPersistentlyFencedBeforeItCanRevive(t *testing.T) {
	s := NewMemory()
	node, err := s.RegisterNodeSession(model.Node{ID: "expired-session", GPUCount: 1, VRAMGB: 24}, "old-session")
	if err != nil {
		t.Fatal(err)
	}
	confirmAgentSession(t, s, node.ID, "old-session")
	s.mu.Lock()
	s.state.Nodes[node.ID].LastHeartbeat = time.Now().Add(-agentSessionActiveFor - time.Second)
	s.mu.Unlock()

	if err := s.HeartbeatNodeSession(node.ID, "old-session"); !errors.Is(err, ErrAgentSession) {
		t.Fatalf("expired heartbeat revived its session: %v", err)
	}
	s.mu.Lock()
	fenced := *s.state.Nodes[node.ID]
	s.mu.Unlock()
	if fenced.SessionEpoch != "" || !fenced.CleanupPending {
		t.Fatalf("expired session fence was not retained: %+v", fenced)
	}
	if _, err := s.ConfirmNodeCleanupSession(node.ID, "old-session"); !errors.Is(err, ErrAgentSession) {
		t.Fatalf("expired session confirmed cleanup: %v", err)
	}

	pending, err := s.RegisterNodeSession(model.Node{ID: node.ID, GPUCount: 1, VRAMGB: 24}, "new-session")
	if err != nil || !pending.CleanupPending {
		t.Fatalf("replacement did not enter cleanup gate: %+v %v", pending, err)
	}
	s.mu.Lock()
	s.state.Nodes[node.ID].LastHeartbeat = time.Now().Add(-agentSessionActiveFor - time.Second)
	s.mu.Unlock()
	if _, err := s.ConfirmNodeCleanupSession(node.ID, "new-session"); !errors.Is(err, ErrAgentSession) {
		t.Fatalf("cleanup confirmation revived an expired registration: %v", err)
	}
}

func TestCleanupPendingRejectsEveryUntrustedAttemptWrite(t *testing.T) {
	s := NewMemory()
	node, _ := s.RegisterNodeSession(model.Node{ID: "pending-writes", GPUCount: 1, VRAMGB: 24}, "session")
	confirmAgentSession(t, s, node.ID, "session")
	job, _ := s.CreateJob(model.JobCreate{Name: "pending writes", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	_ = s.Schedule(time.Minute)
	dispatch, _ := s.NextJobSession(node.ID, "session")
	if _, err := s.UpdateJobLease(job.ID, node.ID, "session", dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterNodeSession(model.Node{ID: node.ID, GPUCount: 1, VRAMGB: 24}, "session"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpdateJobLease(job.ID, node.ID, "session", dispatch.AttemptToken, model.JobUpdate{Status: model.JobSucceeded}); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("terminal status crossed cleanup gate: %v", err)
	}
	if _, err := s.UpdateJobOutputLease(job.ID, node.ID, "session", dispatch.AttemptToken, "late"); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("log write crossed cleanup gate: %v", err)
	}
	if err := s.ValidateJobAttempt(job.ID, node.ID, "session", dispatch.AttemptToken); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("artifact attempt crossed cleanup gate: %v", err)
	}
	stored, _ := s.GetJob(job.ID)
	if stored.Status != model.JobRunning || stored.AssignedNode != node.ID {
		t.Fatalf("pending write released the job: %+v", stored)
	}
}

func TestCreateJobValidatesAndNormalizesCPUOnlyRequirements(t *testing.T) {
	s := NewMemory()
	_, _ = s.RegisterNode(model.Node{ID: "cpu-node", GPUModel: "none", CPUCores: 8})
	job, err := s.CreateJob(model.JobCreate{
		Name:  "cpu",
		Image: "work",
		Requirements: model.Requirements{
			GPUCount: 0, MinVRAMGB: 80, GPUModels: []string{"H100"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.Requirements.MinVRAMGB != 0 || len(job.Requirements.GPUModels) != 0 || job.TimeoutSeconds != 3600 {
		t.Fatalf("CPU-only requirements were not normalized: %+v", job)
	}
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	job, _ = s.GetJob(job.ID)
	if job.Status != model.JobAssigned || job.AssignedNode != "cpu-node" {
		t.Fatalf("normalized CPU-only job was not schedulable: %+v", job)
	}

	overflow := maxPersistedJobInt + 1
	invalid := []model.JobCreate{
		{Name: "negative gpu", Image: "work", Requirements: model.Requirements{GPUCount: -1}},
		{Name: "too many gpus", Image: "work", Requirements: model.Requirements{GPUCount: maxGPUsPerNodeOrJob + 1}},
		{Name: "negative vram", Image: "work", Requirements: model.Requirements{MinVRAMGB: -1}},
		{Name: "negative price", Image: "work", Requirements: model.Requirements{MaxHourly: -1}},
		{Name: "nan price", Image: "work", Requirements: model.Requirements{MaxHourly: math.NaN()}},
		{Name: "negative timeout", Image: "work", TimeoutSeconds: -1},
		{Name: "timeout overflow", Image: "work", TimeoutSeconds: int(overflow)},
		{Name: "negative retries", Image: "work", MaxRetries: -1},
		{Name: "retry overflow", Image: "work", MaxRetries: int(overflow)},
	}
	for _, input := range invalid {
		if _, err := s.CreateJob(input); !errors.Is(err, ErrInvalidResources) {
			t.Fatalf("invalid job %q was accepted: %v", input.Name, err)
		}
	}

	legacy := NewMemory()
	_, _ = legacy.RegisterNode(model.Node{ID: "legacy-cpu-node", CPUCores: 8})
	now := time.Now().UTC()
	legacyJob := &model.Job{ID: "legacy-cpu", Name: "legacy CPU", Image: "work", Requirements: model.Requirements{GPUCount: 0, MinVRAMGB: 80, GPUModels: []string{"H100"}}, Status: model.JobQueued, CreatedAt: now, UpdatedAt: now}
	legacy.mu.Lock()
	legacy.state.Jobs[legacyJob.ID] = legacyJob
	legacy.mu.Unlock()
	if err := legacy.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	legacyJob, _ = legacy.GetJob(legacyJob.ID)
	if legacyJob.Status != model.JobAssigned || legacyJob.AssignedNode != "legacy-cpu-node" {
		t.Fatalf("persisted CPU-only GPU filters still blocked scheduling: %+v", legacyJob)
	}
}

func TestGranularSchedulingDoesNotAllocateFromOversizedGPUCount(t *testing.T) {
	s := NewMemory()
	s.SetGPUGranularScheduling(true)
	_, _ = s.RegisterNode(model.Node{ID: "small", GPUCount: 1, VRAMGB: 24})
	if _, err := s.CreateJob(model.JobCreate{Name: "oversized", Image: "work", Requirements: model.Requirements{GPUCount: math.MaxInt}}); !errors.Is(err, ErrInvalidResources) {
		t.Fatalf("oversized request was accepted: %v", err)
	}
	now := time.Now().UTC()
	job := &model.Job{ID: "legacy-oversized", Name: "legacy oversized", Image: "work", Requirements: model.Requirements{GPUCount: math.MaxInt}, Status: model.JobQueued, CreatedAt: now, UpdatedAt: now}
	s.mu.Lock()
	s.state.Jobs[job.ID] = job
	s.mu.Unlock()
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	job, _ = s.GetJob(job.ID)
	if job.Status != model.JobQueued {
		t.Fatalf("oversized GPU request was assigned: %+v", job)
	}
	legacyNode := &model.Node{ID: "legacy-huge-node", GPUCount: math.MaxInt}
	if available := s.availableGPUsLocked(legacyNode, math.MaxInt); len(available) != maxGPUsPerNodeOrJob {
		t.Fatalf("defensive GPU enumeration returned %d devices", len(available))
	}
}

func TestFailedJobRetries(t *testing.T) {
	s := NewMemory()
	_, _ = s.RegisterNode(model.Node{ID: "node", GPUCount: 1, VRAMGB: 24})
	j, _ := s.CreateJob(model.JobCreate{Name: "retry", Image: "alpine", MaxRetries: 1, Requirements: model.Requirements{GPUCount: 1}})
	_ = s.Schedule(time.Minute)
	_, err := s.UpdateJob(j.ID, "node", model.JobUpdate{Status: model.JobRunning})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.UpdateJob(j.ID, "node", model.JobUpdate{Status: model.JobFailed, Error: "boom"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobQueued {
		t.Fatalf("expected queued, got %s", got.Status)
	}
}

func TestRegisterNodeRecoversRunningJob(t *testing.T) {
	s := NewMemory()
	node, _ := s.RegisterNode(model.Node{ID: "recover-node", GPUCount: 1, VRAMGB: 24})
	job, _ := s.CreateJob(model.JobCreate{Name: "recover", Image: "alpine", MaxRetries: 1, Requirements: model.Requirements{GPUCount: 1}})
	_ = s.Schedule(time.Minute)
	_, _ = s.UpdateJob(job.ID, node.ID, model.JobUpdate{Status: model.JobRunning, Output: "partial"})

	restarted, err := s.RegisterNode(model.Node{ID: node.ID, GPUCount: 1, VRAMGB: 24})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := s.NextJob(restarted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || recovered.ID != job.ID || recovered.Status != model.JobCanceling {
		t.Fatalf("expected interrupted job cleanup dispatch, got %+v", recovered)
	}
	if recovered.Attempts != 1 || recovered.Recoveries != 1 {
		t.Fatalf("unexpected recovery cleanup: %+v", recovered)
	}
	if _, err := s.UpdateJob(job.ID, node.ID, model.JobUpdate{Status: model.JobCanceled}); err != nil {
		t.Fatalf("acknowledge interrupted attempt cleanup: %v", err)
	}
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	recovered, err = s.NextJob(restarted.ID)
	if err != nil || recovered == nil || recovered.Status != model.JobAssigned || recovered.Attempts != 2 || recovered.StartedAt != nil || recovered.Output != "" {
		t.Fatalf("expected cleaned interrupted job to be reassigned, got %+v %v", recovered, err)
	}
	if _, err := s.UpdateJob(job.ID, node.ID, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatalf("restart recovered job: %v", err)
	}
	exhaustedNode, err := s.RegisterNode(model.Node{ID: node.ID, GPUCount: 1, VRAMGB: 24})
	if err != nil {
		t.Fatal(err)
	}
	exhausted, err := s.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.Status != model.JobCanceling || exhausted.Recoveries != 1 || !exhaustedNode.Busy {
		t.Fatalf("expected exhausted recovery to retain its allocation until cleanup: job=%+v node=%+v", exhausted, exhaustedNode)
	}
	// Cleanup dispatch must remain available even when the node can no longer
	// receive new work under the current license.
	s.SetSchedulingLimits(0, 0, time.Now().Add(-time.Minute).Format(time.RFC3339))
	cleanup, err := s.NextJob(exhaustedNode.ID)
	if err != nil || cleanup == nil || cleanup.Status != model.JobCanceling {
		t.Fatalf("expected cleanup dispatch before exhausted failure: %+v %v", cleanup, err)
	}
	if _, err := s.UpdateJob(job.ID, exhaustedNode.ID, model.JobUpdate{Status: model.JobCanceled}); err != nil {
		t.Fatal(err)
	}
	exhausted, _ = s.GetJob(job.ID)
	exhaustedNode = s.ListNodes()[0]
	if exhausted.Status != model.JobFailed || exhaustedNode.Busy || exhaustedNode.CurrentJob != "" {
		t.Fatalf("expected cleanup acknowledgement to fail and release exhausted recovery: job=%+v node=%+v", exhausted, exhaustedNode)
	}
}

func TestScheduleReconcilesOfflineActiveJobs(t *testing.T) {
	t.Run("running job waits for takeover cleanup before retry", func(t *testing.T) {
		s := NewMemory()
		offline, err := s.RegisterNodeSession(model.Node{ID: "offline", GPUCount: 1, VRAMGB: 24, HourlyPrice: 1}, "old-session")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RegisterNodeSession(model.Node{ID: "replacement", GPUCount: 1, VRAMGB: 24, HourlyPrice: 2}, "replacement-session"); err != nil {
			t.Fatal(err)
		}
		confirmAgentSession(t, s, offline.ID, "old-session")
		confirmAgentSession(t, s, "replacement", "replacement-session")
		job, _ := s.CreateJob(model.JobCreate{Name: "retry offline", Image: "work", MaxRetries: 1, Requirements: model.Requirements{GPUCount: 1}})
		if err := s.Schedule(time.Minute); err != nil {
			t.Fatal(err)
		}
		dispatch, err := s.NextJobSession(offline.ID, "old-session")
		if err != nil || dispatch == nil {
			t.Fatalf("dispatch offline attempt: %+v %v", dispatch, err)
		}
		if _, err := s.UpdateJobLease(job.ID, offline.ID, "old-session", dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning, Output: "partial"}); err != nil {
			t.Fatal(err)
		}

		s.mu.Lock()
		s.state.Nodes[offline.ID].LastHeartbeat = time.Now().Add(-2 * time.Minute)
		s.mu.Unlock()
		if err := s.Schedule(time.Minute); err != nil {
			t.Fatal(err)
		}
		held, _ := s.GetJob(job.ID)
		offlineState := s.state.Nodes[offline.ID]
		if held.Status != model.JobRunning || held.AssignedNode != offline.ID || held.Attempts != 1 || held.Recoveries != 0 || !offlineState.CleanupPending || offlineState.SessionEpoch != "" || !offlineState.Busy {
			t.Fatalf("offline attempt was released before cleanup: job=%+v node=%+v", held, offlineState)
		}
		if replacement := s.state.Nodes["replacement"]; replacement.Busy {
			t.Fatalf("replacement received uncleared work: %+v", replacement)
		}
		if _, err := s.NextJobSession("replacement", "replacement-session"); err != nil {
			t.Fatalf("replacement poll failed: %v", err)
		}
		if _, err := s.UpdateJobLease(job.ID, offline.ID, "old-session", dispatch.AttemptToken, model.JobUpdate{Status: model.JobSucceeded}); !errors.Is(err, ErrAgentSession) {
			t.Fatalf("offline attempt was not fenced: %v", err)
		}
		if err := s.HeartbeatNodeSession(offline.ID, "old-session"); !errors.Is(err, ErrAgentSession) {
			t.Fatalf("offline session remained valid: %v", err)
		}
		if err := s.DeleteNode(offline.ID); !errors.Is(err, ErrNodeBusy) {
			t.Fatalf("uncleared offline node was deletable: %v", err)
		}

		returned, err := s.RegisterNodeSession(model.Node{ID: offline.ID, GPUCount: 1, VRAMGB: 24, HourlyPrice: 1}, "new-session")
		if err != nil || !returned.CleanupPending {
			t.Fatalf("returning node did not enter cleanup gate: %+v %v", returned, err)
		}
		if _, err := s.NextJobSession(offline.ID, "new-session"); !errors.Is(err, ErrNodeUnavailable) {
			t.Fatalf("cleanup dispatch escaped pending gate: %v", err)
		}
		confirmAgentSession(t, s, offline.ID, "new-session")
		cleanup, err := s.NextJobSession(offline.ID, "new-session")
		if err != nil || cleanup == nil || cleanup.Status != model.JobCanceling || cleanup.AttemptToken == "" {
			t.Fatalf("recovery cleanup was not dispatched: %+v %v", cleanup, err)
		}
		if _, err := s.UpdateJobLease(job.ID, offline.ID, "new-session", cleanup.AttemptToken, model.JobUpdate{Status: model.JobCanceled}); err != nil {
			t.Fatal(err)
		}
		queued, _ := s.GetJob(job.ID)
		if queued.Status != model.JobQueued || queued.AssignedNode != "" || queued.Recoveries != 1 || queued.Attempts != 1 {
			t.Fatalf("cleanup acknowledgement did not open retry: %+v", queued)
		}
		if err := s.DeleteNode(offline.ID); err != nil {
			t.Fatalf("cleaned node could not be deleted: %v", err)
		}
		if err := s.Schedule(time.Minute); err != nil {
			t.Fatal(err)
		}
		retried, _ := s.GetJob(job.ID)
		if retried.Status != model.JobAssigned || retried.AssignedNode != "replacement" || retried.Attempts != 2 {
			t.Fatalf("cleaned attempt did not retry on replacement: %+v", retried)
		}
	})

	t.Run("exhausted running job fails only after cleanup acknowledgement", func(t *testing.T) {
		s := NewMemory()
		node, _ := s.RegisterNodeSession(model.Node{ID: "exhausted", GPUCount: 1, VRAMGB: 24}, "old-session")
		confirmAgentSession(t, s, node.ID, "old-session")
		job, _ := s.CreateJob(model.JobCreate{Name: "fail offline", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
		_ = s.Schedule(time.Minute)
		dispatch, _ := s.NextJobSession(node.ID, "old-session")
		_, _ = s.UpdateJobLease(job.ID, node.ID, "old-session", dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning, Output: "partial"})
		s.mu.Lock()
		s.state.Nodes[node.ID].LastHeartbeat = time.Now().Add(-2 * time.Minute)
		s.mu.Unlock()
		if err := s.Schedule(time.Minute); err != nil {
			t.Fatal(err)
		}
		held, _ := s.GetJob(job.ID)
		if held.Status != model.JobRunning || held.FinishedAt != nil || !s.ListNodes()[0].Busy {
			t.Fatalf("exhausted attempt closed without cleanup: %+v", held)
		}
		if _, err := s.RegisterNodeSession(model.Node{ID: node.ID, GPUCount: 1, VRAMGB: 24}, "new-session"); err != nil {
			t.Fatal(err)
		}
		confirmAgentSession(t, s, node.ID, "new-session")
		cleanup, err := s.NextJobSession(node.ID, "new-session")
		if err != nil || cleanup == nil || cleanup.Status != model.JobCanceling {
			t.Fatalf("exhausted cleanup was not dispatched: %+v %v", cleanup, err)
		}
		if _, err := s.UpdateJobLease(job.ID, node.ID, "new-session", cleanup.AttemptToken, model.JobUpdate{Status: model.JobCanceled}); err != nil {
			t.Fatal(err)
		}
		failed, _ := s.GetJob(job.ID)
		if failed.Status != model.JobFailed || failed.FinishedAt == nil || s.ListNodes()[0].Busy {
			t.Fatalf("cleanup acknowledgement did not fail exhausted attempt: %+v", failed)
		}
	})

	t.Run("canceling job waits for cleanup acknowledgement", func(t *testing.T) {
		s := NewMemory()
		node, _ := s.RegisterNodeSession(model.Node{ID: "canceling", GPUCount: 1, VRAMGB: 24}, "old-session")
		confirmAgentSession(t, s, node.ID, "old-session")
		job, _ := s.CreateJob(model.JobCreate{Name: "cancel offline", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
		_ = s.Schedule(time.Minute)
		dispatch, _ := s.NextJobSession(node.ID, "old-session")
		_, _ = s.UpdateJobLease(job.ID, node.ID, "old-session", dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning})
		_, _ = s.CancelJob(job.ID)
		s.mu.Lock()
		s.state.Nodes[node.ID].LastHeartbeat = time.Now().Add(-2 * time.Minute)
		s.mu.Unlock()
		if err := s.Schedule(time.Minute); err != nil {
			t.Fatal(err)
		}
		held, _ := s.GetJob(job.ID)
		if held.Status != model.JobCanceling || !s.ListNodes()[0].Busy {
			t.Fatalf("offline cancellation closed without cleanup: %+v", held)
		}
		if _, err := s.RegisterNodeSession(model.Node{ID: node.ID, GPUCount: 1, VRAMGB: 24}, "new-session"); err != nil {
			t.Fatal(err)
		}
		confirmAgentSession(t, s, node.ID, "new-session")
		cleanup, err := s.NextJobSession(node.ID, "new-session")
		if err != nil || cleanup == nil || cleanup.Status != model.JobCanceling {
			t.Fatalf("cancel cleanup was not dispatched: %+v %v", cleanup, err)
		}
		if _, err := s.UpdateJobLease(job.ID, node.ID, "new-session", cleanup.AttemptToken, model.JobUpdate{Status: model.JobCanceled}); err != nil {
			t.Fatal(err)
		}
		canceled, _ := s.GetJob(job.ID)
		if canceled.Status != model.JobCanceled || canceled.FinishedAt == nil || s.ListNodes()[0].Busy {
			t.Fatalf("cleanup acknowledgement did not cancel job: %+v", canceled)
		}
	})

	t.Run("unstarted assignment is fenced and redelivered only after cleanup", func(t *testing.T) {
		s := NewMemory()
		node, _ := s.RegisterNodeSession(model.Node{ID: "assigned", GPUCount: 1, VRAMGB: 24}, "old-session")
		confirmAgentSession(t, s, node.ID, "old-session")
		job, _ := s.CreateJob(model.JobCreate{Name: "assigned offline", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
		_ = s.Schedule(time.Minute)
		first, _ := s.NextJobSession(node.ID, "old-session")
		s.mu.Lock()
		s.state.Nodes[node.ID].LastHeartbeat = time.Now().Add(-2 * time.Minute)
		s.mu.Unlock()
		if err := s.Schedule(time.Minute); err != nil {
			t.Fatal(err)
		}
		held, _ := s.GetJob(job.ID)
		if held.Status != model.JobAssigned || held.AssignedNode != node.ID || held.Attempts != 1 || !s.ListNodes()[0].Busy {
			t.Fatalf("unstarted assignment was released before cleanup: %+v", held)
		}
		if _, err := s.UpdateJobLease(job.ID, node.ID, "old-session", first.AttemptToken, model.JobUpdate{Status: model.JobRunning}); !errors.Is(err, ErrAgentSession) {
			t.Fatalf("old assignment was not fenced: %v", err)
		}
		if _, err := s.RegisterNodeSession(model.Node{ID: node.ID, GPUCount: 1, VRAMGB: 24}, "new-session"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.NextJobSession(node.ID, "new-session"); !errors.Is(err, ErrNodeUnavailable) {
			t.Fatalf("assignment escaped cleanup gate: %v", err)
		}
		confirmAgentSession(t, s, node.ID, "new-session")
		second, err := s.NextJobSession(node.ID, "new-session")
		if err != nil || second == nil || second.Status != model.JobAssigned || second.Attempts != 1 || second.AttemptToken == first.AttemptToken {
			t.Fatalf("cleaned assignment was not safely redelivered: %+v %v", second, err)
		}
	})
}

func TestCancelOverridesRecoveryRetryAfterTakeover(t *testing.T) {
	s := NewMemory()
	node, _ := s.RegisterNode(model.Node{ID: "cancel-recovery", GPUCount: 1, VRAMGB: 24})
	job, _ := s.CreateJob(model.JobCreate{Name: "cancel recovery", Image: "alpine", MaxRetries: 1, Requirements: model.Requirements{GPUCount: 1}})
	_ = s.Schedule(time.Minute)
	_, _ = s.UpdateJob(job.ID, node.ID, model.JobUpdate{Status: model.JobRunning})
	if _, err := s.RegisterNode(model.Node{ID: node.ID, GPUCount: 1, VRAMGB: 24}); err != nil {
		t.Fatal(err)
	}
	if canceled, err := s.CancelJob(job.ID); err != nil || canceled.Status != model.JobCanceling {
		t.Fatalf("cancel cleanup-only recovery: %+v %v", canceled, err)
	}
	cleanup, err := s.NextJob(node.ID)
	if err != nil || cleanup == nil || cleanup.Status != model.JobCanceling {
		t.Fatalf("dispatch canceled recovery cleanup: %+v %v", cleanup, err)
	}
	if _, err := s.UpdateJob(job.ID, node.ID, model.JobUpdate{Status: model.JobCanceled}); err != nil {
		t.Fatal(err)
	}
	final, _ := s.GetJob(job.ID)
	if final.Status != model.JobCanceled {
		t.Fatalf("explicit cancellation was lost to recovery retry: %+v", final)
	}
}

func TestDeleteNodeRejectsActiveJob(t *testing.T) {
	s := NewMemory()
	_, _ = s.RegisterNode(model.Node{ID: "node", GPUCount: 1, VRAMGB: 24})
	job, _ := s.CreateJob(model.JobCreate{Name: "active", Image: "alpine", Requirements: model.Requirements{GPUCount: 1}})
	_ = s.Schedule(time.Minute)
	if err := s.DeleteNode("node"); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("expected ErrNodeBusy, got %v", err)
	}
	_, _ = s.UpdateJob(job.ID, "node", model.JobUpdate{Status: model.JobRunning})
	_, _ = s.UpdateJob(job.ID, "node", model.JobUpdate{Status: model.JobSucceeded})
	if err := s.DeleteNode("node"); err != nil {
		t.Fatalf("delete completed node: %v", err)
	}
	if _, err := s.RegisterNode(model.Node{ID: "replacement"}); err != nil {
		t.Fatal(err)
	}
}

func TestCancelRerunDeleteAndQueryJobs(t *testing.T) {
	s := NewMemory()
	node, _ := s.RegisterNode(model.Node{ID: "cancel-node", Pool: "default", GPUCount: 1, VRAMGB: 8})
	job, _ := s.CreateJob(model.JobCreate{Name: "searchable training", Image: "alpine", Requirements: model.Requirements{GPUCount: 1, Pools: []string{"default"}}})
	_ = s.Schedule(time.Minute)
	_, _ = s.UpdateJob(job.ID, node.ID, model.JobUpdate{Status: model.JobRunning})
	canceling, err := s.CancelJob(job.ID)
	if err != nil || canceling.Status != model.JobCanceling {
		t.Fatalf("cancel: %+v %v", canceling, err)
	}
	canceled, err := s.UpdateJob(job.ID, node.ID, model.JobUpdate{Status: model.JobCanceled})
	if err != nil || canceled.Status != model.JobCanceled {
		t.Fatalf("ack cancel: %+v %v", canceled, err)
	}
	rerun, err := s.RerunJob(job.ID)
	if err != nil || rerun.RerunOf != job.ID {
		t.Fatalf("rerun: %+v %v", rerun, err)
	}
	page := s.QueryJobs(JobQuery{Search: "training", Status: "canceled", Page: 1, PageSize: 1})
	if page.Total != 1 || page.Items[0].ID != job.ID {
		t.Fatalf("query: %+v", page)
	}
	if err := s.DeleteJob(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetJob(job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected deleted job, got %v", err)
	}
	if err := s.DeleteJob(rerun.ID); !errors.Is(err, ErrJobActive) {
		t.Fatalf("expected active conflict, got %v", err)
	}
}

func TestUpdateJobOutputWhileRunning(t *testing.T) {
	s := NewMemory()
	node, _ := s.RegisterNode(model.Node{ID: "log-node", GPUCount: 1})
	job, _ := s.CreateJob(model.JobCreate{Name: "logs", Image: "alpine"})
	_ = s.Schedule(time.Minute)
	_, _ = s.UpdateJob(job.ID, node.ID, model.JobUpdate{Status: model.JobRunning})

	updated, err := s.UpdateJobOutput(job.ID, node.ID, strings.Repeat("x", 70<<10))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != model.JobRunning || len(updated.Output) != 64<<10 {
		t.Fatalf("unexpected live log update: status=%s bytes=%d", updated.Status, len(updated.Output))
	}
	if _, err := s.UpdateJobOutput(job.ID, "other-node", "forbidden"); err == nil {
		t.Fatal("expected another node's log update to fail")
	}
}

func TestGPUGranularSchedulingAllocatesDistinctDevices(t *testing.T) {
	s := NewMemory()
	s.SetGPUGranularScheduling(true)
	node, _ := s.RegisterNode(model.Node{ID: "four-gpu", GPUCount: 4, VRAMGB: 24})
	first, _ := s.CreateJob(model.JobCreate{Name: "first", Image: "train", Requirements: model.Requirements{GPUCount: 2}})
	second, _ := s.CreateJob(model.JobCreate{Name: "second", Image: "train", Requirements: model.Requirements{GPUCount: 2}})
	third, _ := s.CreateJob(model.JobCreate{Name: "third", Image: "train", Requirements: model.Requirements{GPUCount: 1}})
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}

	first, _ = s.GetJob(first.ID)
	second, _ = s.GetJob(second.ID)
	third, _ = s.GetJob(third.ID)
	if first.Status != model.JobAssigned || second.Status != model.JobAssigned || third.Status != model.JobQueued {
		t.Fatalf("unexpected statuses: %s %s %s", first.Status, second.Status, third.Status)
	}
	allAllocated := append(append([]int(nil), first.AllocatedGPUs...), second.AllocatedGPUs...)
	sort.Ints(allAllocated)
	if !reflect.DeepEqual(allAllocated, []int{0, 1, 2, 3}) {
		t.Fatalf("overlapping allocations: %v %v", first.AllocatedGPUs, second.AllocatedGPUs)
	}
	claimedOne, _ := s.NextJob(node.ID)
	claimedTwo, _ := s.NextJob(node.ID)
	if claimedOne == nil || claimedTwo == nil || claimedOne.ID == claimedTwo.ID {
		t.Fatalf("concurrent workers did not claim distinct jobs: %+v %+v", claimedOne, claimedTwo)
	}
	_, _ = s.UpdateJob(first.ID, node.ID, model.JobUpdate{Status: model.JobRunning})
	_, _ = s.UpdateJob(first.ID, node.ID, model.JobUpdate{Status: model.JobSucceeded})
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	third, _ = s.GetJob(third.ID)
	if third.Status != model.JobAssigned || !reflect.DeepEqual(third.AllocatedGPUs, []int{0}) {
		t.Fatalf("released GPU was not reused: %+v", third)
	}
	nodes := s.ListNodes()
	if len(nodes) != 1 || nodes[0].AllocatedGPUs != 3 || len(nodes[0].ActiveJobs) != 2 {
		t.Fatalf("unexpected node usage: %+v", nodes)
	}
}

func TestCommunitySchedulingRemainsWholeNodeExclusive(t *testing.T) {
	s := NewMemory()
	_, _ = s.RegisterNode(model.Node{ID: "community-four-gpu", GPUCount: 4, VRAMGB: 24})
	first, _ := s.CreateJob(model.JobCreate{Name: "first", Image: "train", Requirements: model.Requirements{GPUCount: 1}})
	second, _ := s.CreateJob(model.JobCreate{Name: "second", Image: "train", Requirements: model.Requirements{GPUCount: 1}})
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	first, _ = s.GetJob(first.ID)
	second, _ = s.GetJob(second.ID)
	assigned, queued := 0, 0
	for _, job := range []*model.Job{first, second} {
		if job.Status == model.JobAssigned && len(job.AllocatedGPUs) == 0 {
			assigned++
		}
		if job.Status == model.JobQueued {
			queued++
		}
	}
	if assigned != 1 || queued != 1 {
		t.Fatalf("community scheduling stopped being whole-node exclusive: %+v %+v", first, second)
	}
}

func TestGPUGranularSchedulingKeepsCPUOnlyJobsWholeNodeExclusive(t *testing.T) {
	s := NewMemory()
	s.SetGPUGranularScheduling(true)
	_, _ = s.RegisterNode(model.Node{ID: "mixed-node", GPUCount: 4, CPUCores: 32, VRAMGB: 24})
	first, _ := s.CreateJob(model.JobCreate{Name: "cpu-one", Image: "work", Requirements: model.Requirements{GPUCount: 0}})
	second, _ := s.CreateJob(model.JobCreate{Name: "cpu-two", Image: "work", Requirements: model.Requirements{GPUCount: 0}})
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	first, _ = s.GetJob(first.ID)
	second, _ = s.GetJob(second.ID)
	assigned, queued := 0, 0
	for _, job := range []*model.Job{first, second} {
		if job.Status == model.JobAssigned {
			assigned++
		}
		if job.Status == model.JobQueued {
			queued++
		}
	}
	if assigned != 1 || queued != 1 {
		t.Fatalf("CPU-only jobs were assigned concurrently: %+v %+v", first, second)
	}
	gpu, _ := s.CreateJob(model.JobCreate{Name: "gpu-after-cpu", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	gpu, _ = s.GetJob(gpu.ID)
	if gpu.Status != model.JobQueued {
		t.Fatalf("GPU job overlapped whole-node CPU job: %+v", gpu)
	}
}

func TestSchedulingLimitsReconcilePersistedNodesAndExpiration(t *testing.T) {
	s := NewMemory()
	_, _ = s.RegisterNode(model.Node{ID: "a", GPUCount: 1, VRAMGB: 24})
	_, _ = s.RegisterNode(model.Node{ID: "b", GPUCount: 1, VRAMGB: 24})
	s.SetSchedulingLimits(1, 1, "")
	job, _ := s.CreateJob(model.JobCreate{Name: "within-limit", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	job, _ = s.GetJob(job.ID)
	if job.AssignedNode != "a" {
		t.Fatalf("expected stable licensed node a, got %+v", job)
	}
	_, _ = s.UpdateJob(job.ID, "a", model.JobUpdate{Status: model.JobRunning})
	_, _ = s.UpdateJob(job.ID, "a", model.JobUpdate{Status: model.JobSucceeded})
	s.SetSchedulingLimits(1, 1, time.Now().Add(-time.Minute).Format(time.RFC3339))
	expired, _ := s.CreateJob(model.JobCreate{Name: "expired", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	expired, _ = s.GetJob(expired.ID)
	if expired.Status != model.JobQueued {
		t.Fatalf("expired license scheduled new work: %+v", expired)
	}
}

func TestDegradedNodeStopsAndRecoversScheduling(t *testing.T) {
	s := NewMemory()
	_, _ = s.RegisterNode(model.Node{ID: "health-node", GPUCount: 1, VRAMGB: 24})
	if _, err := s.UpdateNodeHealth("health-node", model.NodeHealthUpdate{Status: "DEGRADED", Reason: "runtime check failed"}); err != nil {
		t.Fatal(err)
	}
	job, _ := s.CreateJob(model.JobCreate{Name: "wait-for-health", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	_ = s.Schedule(time.Minute)
	job, _ = s.GetJob(job.ID)
	if job.Status != model.JobQueued {
		t.Fatalf("degraded node received work: %+v", job)
	}
	if _, err := s.UpdateNodeHealth("health-node", model.NodeHealthUpdate{Status: "HEALTHY", GPUModel: "L4", GPUCount: 1, VRAMGB: 24}); err != nil {
		t.Fatal(err)
	}
	_ = s.Schedule(time.Minute)
	job, _ = s.GetJob(job.ID)
	if job.Status != model.JobAssigned {
		t.Fatalf("recovered node did not receive work: %+v", job)
	}
}

func gpuInventory(modelName string, count, vram int) []model.GPUDevice {
	devices := make([]model.GPUDevice, count)
	for index := range devices {
		devices[index] = model.GPUDevice{Index: index, UUID: "GPU-" + string(rune('A'+index)), Model: modelName, VRAMGB: vram}
	}
	return devices
}

func TestNodeInventoryAndLicenseAdmissionAreAtomic(t *testing.T) {
	s := NewMemory()
	s.SetSchedulingLimits(0, 1, "")
	s.SetPerGPUInventory(true)
	s.SetNodeHealthPolicy(true, 3*time.Minute)
	for _, id := range []string{"a", "b"} {
		if _, err := s.RegisterNodeSession(model.Node{ID: id, GPUModel: "none", HealthStatus: "HEALTHY"}, "session-"+id); err != nil {
			t.Fatal(err)
		}
		confirmAgentSession(t, s, id, "session-"+id)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var workers sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		id := id
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := s.UpdateNodeHealthSession(id, "session-"+id, model.NodeHealthUpdate{
				Status: "HEALTHY", GPUModel: "L4", GPUCount: 1, VRAMGB: 24,
				Devices: gpuInventory("L4", 1, 24),
			})
			errs <- err
		}()
	}
	close(start)
	workers.Wait()
	close(errs)
	accepted, rejected := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrLicenseCapacity):
			rejected++
		default:
			t.Fatalf("unexpected health admission error: %v", err)
		}
	}
	total := 0
	for _, node := range s.ListNodes() {
		total += node.GPUCount
	}
	if accepted != 1 || rejected != 1 || total != 1 {
		t.Fatalf("health admission was not atomic: accepted=%d rejected=%d GPUs=%d", accepted, rejected, total)
	}

	if _, err := s.UpdateNodeHealthSession("a", "session-a", model.NodeHealthUpdate{Status: "DEGRADED", GPUCount: -1}); !errors.Is(err, ErrInvalidResources) {
		t.Fatalf("negative health inventory was accepted: %v", err)
	}
	bad := model.Node{ID: "bad", GPUModel: "L4", GPUCount: 2, VRAMGB: 24, HealthStatus: "HEALTHY", Devices: []model.GPUDevice{{Index: 0, UUID: "same", Model: "L4", VRAMGB: 24}, {Index: 0, UUID: "same", Model: "L4", VRAMGB: 24}}}
	if _, err := s.RegisterNodeSession(bad, "bad-session"); !errors.Is(err, ErrInvalidResources) {
		t.Fatalf("inconsistent per-GPU inventory was accepted: %v", err)
	}
}

func TestExpiredUnlimitedLicenseRejectsOnlyNewCapacity(t *testing.T) {
	s := NewMemory()
	if _, err := s.RegisterNodeSession(model.Node{ID: "existing", GPUCount: 1, VRAMGB: 24}, "same-session"); err != nil {
		t.Fatal(err)
	}
	confirmAgentSession(t, s, "existing", "same-session")
	s.SetSchedulingLimits(0, 0, time.Now().Add(-time.Minute).Format(time.RFC3339))
	if _, err := s.RegisterNodeSession(model.Node{ID: "existing", GPUCount: 1, VRAMGB: 24}, "same-session"); err != nil {
		t.Fatalf("same-capacity reconnect failed: %v", err)
	}
	confirmAgentSession(t, s, "existing", "same-session")
	if _, err := s.RegisterNodeSession(model.Node{ID: "new"}, "new-session"); !errors.Is(err, ErrLicenseExpired) {
		t.Fatalf("expired unlimited license accepted a new node: %v", err)
	}
	if _, err := s.UpdateNodeHealthSession("existing", "same-session", model.NodeHealthUpdate{Status: "HEALTHY", GPUModel: "L4", GPUCount: 2, VRAMGB: 24}); !errors.Is(err, ErrLicenseExpired) {
		t.Fatalf("expired unlimited license accepted GPU growth: %v", err)
	}
}

func TestHealthAndLicenseAreRecheckedBeforeStart(t *testing.T) {
	s := NewMemory()
	s.SetNodeHealthPolicy(true, 3*time.Minute)
	node, err := s.RegisterNodeSession(model.Node{ID: "guarded", GPUCount: 1, VRAMGB: 24, HealthStatus: "HEALTHY"}, "session")
	if err != nil {
		t.Fatal(err)
	}
	confirmAgentSession(t, s, node.ID, "session")
	job, _ := s.CreateJob(model.JobCreate{Name: "guarded", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateNodeHealthSession(node.ID, "session", model.NodeHealthUpdate{Status: "DEGRADED", Reason: "runtime failed"}); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.GetJob(job.ID)
	if stored.Status != model.JobQueued || stored.AssignedNode != "" || stored.Attempts != 0 {
		t.Fatalf("degraded assignment was not rolled back: %+v", stored)
	}

	if _, err := s.UpdateNodeHealthSession(node.ID, "session", model.NodeHealthUpdate{Status: "HEALTHY", GPUModel: "L4", GPUCount: 1, VRAMGB: 24}); err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	stale := time.Now().Add(-10 * time.Minute)
	s.state.Nodes[node.ID].LastHealthCheck = &stale
	s.mu.Unlock()
	if _, err := s.NextJobSession(node.ID, "session"); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("stale health did not block dispatch: %v", err)
	}
	stored, _ = s.GetJob(job.ID)
	if stored.Status != model.JobQueued || stored.AssignedNode != "" {
		t.Fatalf("stale assignment was not rolled back: %+v", stored)
	}

	s.mu.Lock()
	fresh := time.Now().UTC()
	s.state.Nodes[node.ID].LastHealthCheck = &fresh
	s.mu.Unlock()
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	s.SetSchedulingLimits(0, 0, time.Now().Add(-time.Minute).Format(time.RFC3339))
	if _, err := s.NextJobSession(node.ID, "session"); !errors.Is(err, ErrNodeUnavailable) {
		t.Fatalf("expired license did not block dispatch: %v", err)
	}
	stored, _ = s.GetJob(job.ID)
	if stored.Status != model.JobQueued {
		t.Fatalf("unlicensed assignment was not rolled back: %+v", stored)
	}
}

func TestAgentSessionAndAttemptLeaseFenceLateWriters(t *testing.T) {
	s := NewMemory()
	node, err := s.RegisterNodeSession(model.Node{ID: "fenced", GPUCount: 1, VRAMGB: 24}, "session-one")
	if err != nil {
		t.Fatal(err)
	}
	confirmAgentSession(t, s, node.ID, "session-one")
	job, _ := s.CreateJob(model.JobCreate{Name: "fenced", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	_ = s.Schedule(time.Minute)
	first, err := s.NextJobSession(node.ID, "session-one")
	if err != nil || first == nil {
		t.Fatalf("first claim failed: %+v %v", first, err)
	}
	if _, err := s.RegisterNodeSession(model.Node{ID: node.ID, GPUCount: 1, VRAMGB: 24}, "session-two"); !errors.Is(err, ErrAgentSessionActive) {
		t.Fatalf("live duplicate session was accepted: %v", err)
	}
	s.mu.Lock()
	s.state.Nodes[node.ID].LastHeartbeat = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	if _, err := s.RegisterNodeSession(model.Node{ID: node.ID, GPUCount: 1, VRAMGB: 24}, "session-two"); err != nil {
		t.Fatal(err)
	}
	confirmAgentSession(t, s, node.ID, "session-two")
	if err := s.HeartbeatNodeSession(node.ID, "session-one"); !errors.Is(err, ErrAgentSession) {
		t.Fatalf("stale session heartbeat was accepted: %v", err)
	}
	second, err := s.NextJobSession(node.ID, "session-two")
	if err != nil || second == nil || second.AttemptToken == first.AttemptToken {
		t.Fatalf("new session did not receive a new attempt: %+v %v", second, err)
	}
	if _, err := s.UpdateJobLease(job.ID, node.ID, "session-one", first.AttemptToken, model.JobUpdate{Status: model.JobRunning}); !errors.Is(err, ErrAgentSession) {
		t.Fatalf("stale attempt started the job: %v", err)
	}
	if _, err := s.UpdateJobLease(job.ID, node.ID, "session-two", second.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatalf("current attempt could not start: %v", err)
	}
	if _, err := s.UpdateJobOutputLease(job.ID, node.ID, "session-one", first.AttemptToken, "late"); !errors.Is(err, ErrAgentSession) {
		t.Fatalf("stale attempt wrote logs: %v", err)
	}
}

func TestCancelKeepsRunningLeaseUntilOwnerAcknowledges(t *testing.T) {
	s := NewMemory()
	node, _ := s.RegisterNodeSession(model.Node{ID: "cancel-fenced", GPUCount: 1, VRAMGB: 24}, "session")
	confirmAgentSession(t, s, node.ID, "session")
	job, _ := s.CreateJob(model.JobCreate{Name: "cancel", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	_ = s.Schedule(time.Minute)
	dispatch, _ := s.NextJobSession(node.ID, "session")
	if _, err := s.UpdateJobLease(job.ID, node.ID, "session", dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatal(err)
	}
	canceling, err := s.CancelJob(job.ID)
	if err != nil || canceling.Status != model.JobCanceling {
		t.Fatalf("cancel did not enter canceling: %+v %v", canceling, err)
	}
	if !s.ListNodes()[0].Busy {
		t.Fatal("running cancellation released the node before container stop acknowledgement")
	}
	if next, err := s.NextJobSession(node.ID, "session"); err != nil || next != nil {
		t.Fatalf("another worker stole running cancellation: %+v %v", next, err)
	}
	if _, err := s.UpdateJobLease(job.ID, node.ID, "session", "wrong", model.JobUpdate{Status: model.JobCanceled}); !errors.Is(err, ErrAttemptLease) {
		t.Fatalf("wrong worker acknowledged cancellation: %v", err)
	}
	if _, err := s.UpdateJobLease(job.ID, node.ID, "session", dispatch.AttemptToken, model.JobUpdate{Status: model.JobCanceled}); err != nil {
		t.Fatal(err)
	}
	if s.ListNodes()[0].Busy {
		t.Fatal("node remained busy after owner acknowledged container stop")
	}
}
