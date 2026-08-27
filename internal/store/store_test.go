package store

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuflow/internal/model"
)

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
	s.SetSchedulingLimits(0, 0, time.Now().Add(-time.Minute).Format(time.RFC3339))
	if _, err := s.RegisterNodeSession(model.Node{ID: "existing", GPUCount: 1, VRAMGB: 24}, "same-session"); err != nil {
		t.Fatalf("same-capacity reconnect failed: %v", err)
	}
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
	if err := s.HeartbeatNodeSession(node.ID, "session-one"); !errors.Is(err, ErrAgentSession) {
		t.Fatalf("stale session heartbeat was accepted: %v", err)
	}
	second, err := s.NextJobSession(node.ID, "session-two")
	if err != nil || second == nil || second.AttemptToken == first.AttemptToken {
		t.Fatalf("new session did not receive a new attempt: %+v %v", second, err)
	}
	if _, err := s.UpdateJobLease(job.ID, node.ID, "session-one", first.AttemptToken, model.JobUpdate{Status: model.JobRunning}); !errors.Is(err, ErrAttemptLease) {
		t.Fatalf("stale attempt started the job: %v", err)
	}
	if _, err := s.UpdateJobLease(job.ID, node.ID, "session-two", second.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatalf("current attempt could not start: %v", err)
	}
	if _, err := s.UpdateJobOutputLease(job.ID, node.ID, "session-one", first.AttemptToken, "late"); !errors.Is(err, ErrAttemptLease) {
		t.Fatalf("stale attempt wrote logs: %v", err)
	}
}

func TestCancelKeepsRunningLeaseUntilOwnerAcknowledges(t *testing.T) {
	s := NewMemory()
	node, _ := s.RegisterNodeSession(model.Node{ID: "cancel-fenced", GPUCount: 1, VRAMGB: 24}, "session")
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
