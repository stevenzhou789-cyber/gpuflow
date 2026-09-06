package store

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"gpuflow/internal/model"
)

func createWeightedProject(t *testing.T, state *Store, id string, weight int) *model.Project {
	t.Helper()
	project, err := state.CreateProject(model.ProjectCreate{ID: id, Name: id, Weight: weight})
	if err != nil {
		t.Fatalf("create project %s: %v", id, err)
	}
	return project
}

func queueFairJobs(t *testing.T, state *Store, projectID, prefix string, count, gpuCount, priority int) {
	t.Helper()
	for index := 0; index < count; index++ {
		if _, err := state.CreateJobForProject(projectID, model.JobCreate{
			Name: fmt.Sprintf("%s-%03d", prefix, index), Image: "work", Priority: priority,
			Requirements: model.Requirements{GPUCount: gpuCount},
		}); err != nil {
			t.Fatalf("queue job %s/%d: %v", projectID, index, err)
		}
	}
}

func scheduleAndCompleteOne(t *testing.T, state *Store) *model.Job {
	t.Helper()
	if err := state.Schedule(time.Minute); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	var assigned *model.Job
	for _, job := range state.ListJobs() {
		if job.Status != model.JobAssigned {
			continue
		}
		if assigned != nil {
			t.Fatalf("expected one assignment, got at least %s and %s", assigned.ID, job.ID)
		}
		assigned = job
	}
	if assigned == nil {
		t.Fatal("expected one assignment")
	}
	if _, err := state.UpdateJob(assigned.ID, assigned.AssignedNode, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatalf("start %s: %v", assigned.ID, err)
	}
	if _, err := state.UpdateJob(assigned.ID, assigned.AssignedNode, model.JobUpdate{Status: model.JobSucceeded}); err != nil {
		t.Fatalf("finish %s: %v", assigned.ID, err)
	}
	return assigned
}

func absoluteInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func TestFairSchedulerEqualWeightsShareGPUDispatches(t *testing.T) {
	state := NewMemory()
	state.SetProjectFairScheduling(true)
	createWeightedProject(t, state, "alpha", 1)
	createWeightedProject(t, state, "beta", 1)
	if _, err := state.RegisterNode(model.Node{ID: "worker", GPUCount: 1, VRAMGB: 24}); err != nil {
		t.Fatal(err)
	}
	queueFairJobs(t, state, "alpha", "a", 30, 1, 0)
	queueFairJobs(t, state, "beta", "b", 30, 1, 0)

	counts := map[string]int{}
	for index := 0; index < 40; index++ {
		counts[scheduleAndCompleteOne(t, state).ProjectID]++
	}
	if absoluteInt(counts["alpha"]-counts["beta"]) > 1 {
		t.Fatalf("equal weights did not share GPU dispatches: %+v", counts)
	}
}

func TestFairSchedulerWeightThreeToOne(t *testing.T) {
	state := NewMemory()
	state.SetProjectFairScheduling(true)
	createWeightedProject(t, state, "heavy", 3)
	createWeightedProject(t, state, "light", 1)
	if _, err := state.RegisterNode(model.Node{ID: "worker", GPUCount: 1, VRAMGB: 24}); err != nil {
		t.Fatal(err)
	}
	queueFairJobs(t, state, "heavy", "h", 50, 1, 0)
	queueFairJobs(t, state, "light", "l", 50, 1, 0)

	counts := map[string]int{}
	for index := 0; index < 40; index++ {
		counts[scheduleAndCompleteOne(t, state).ProjectID]++
	}
	if absoluteInt(counts["heavy"]-3*counts["light"]) > 4 {
		t.Fatalf("3:1 weight did not converge by GPU dispatch cost: %+v", counts)
	}
}

func TestFairSchedulerChargesRequestedGPUs(t *testing.T) {
	state := NewMemory()
	state.SetProjectFairScheduling(true)
	createWeightedProject(t, state, "large", 1)
	createWeightedProject(t, state, "small", 1)
	if _, err := state.RegisterNode(model.Node{ID: "worker", GPUCount: 4, VRAMGB: 24}); err != nil {
		t.Fatal(err)
	}
	queueFairJobs(t, state, "large", "large", 20, 4, 0)
	queueFairJobs(t, state, "small", "small", 40, 1, 0)

	gpuCost := map[string]int{}
	jobCount := map[string]int{}
	for index := 0; index < 25; index++ {
		job := scheduleAndCompleteOne(t, state)
		gpuCost[job.ProjectID] += job.Requirements.GPUCount
		jobCount[job.ProjectID]++
	}
	if absoluteInt(gpuCost["large"]-gpuCost["small"]) > 4 {
		t.Fatalf("equal projects diverged by more than one largest job: costs=%+v jobs=%+v", gpuCost, jobCount)
	}
	if jobCount["small"] <= jobCount["large"] {
		t.Fatalf("scheduler counted jobs instead of requested GPUs: costs=%+v jobs=%+v", gpuCost, jobCount)
	}
}

func TestFairSchedulerUsesPriorityOnlyInsideProject(t *testing.T) {
	state := NewMemory()
	state.SetProjectFairScheduling(true)
	createWeightedProject(t, state, "alpha", 1)
	if _, err := state.RegisterNode(model.Node{ID: "worker", GPUCount: 1, VRAMGB: 24}); err != nil {
		t.Fatal(err)
	}
	low, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "low", Image: "work", Priority: 1, Requirements: model.Requirements{GPUCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	high, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "high", Image: "work", Priority: 100, Requirements: model.Requirements{GPUCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	gotHigh, _ := state.GetJob(high.ID)
	gotLow, _ := state.GetJob(low.ID)
	if gotHigh.Status != model.JobAssigned || gotLow.Status != model.JobQueued {
		t.Fatalf("project priority order was not honored: high=%+v low=%+v", gotHigh, gotLow)
	}

	other := NewMemory()
	other.SetProjectFairScheduling(true)
	createWeightedProject(t, other, "alpha", 1)
	createWeightedProject(t, other, "beta", 1)
	if _, err := other.RegisterNode(model.Node{ID: "worker", GPUCount: 1, VRAMGB: 24}); err != nil {
		t.Fatal(err)
	}
	alpha, _ := other.CreateJobForProject("alpha", model.JobCreate{Name: "normal", Image: "work", Priority: 0, Requirements: model.Requirements{GPUCount: 1}})
	_, _ = other.CreateJobForProject("beta", model.JobCreate{Name: "urgent", Image: "work", Priority: 100, Requirements: model.Requirements{GPUCount: 1}})
	if err := other.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	gotAlpha, _ := other.GetJob(alpha.ID)
	if gotAlpha.Status != model.JobAssigned {
		t.Fatal("another project's priority bypassed the project vruntime/project-id ordering")
	}
}

func TestFairSchedulerSkipsBlockedVendorWithoutCharging(t *testing.T) {
	state := NewMemory()
	state.SetProjectFairScheduling(true)
	state.SetHeterogeneousAccelerators(true)
	createWeightedProject(t, state, "ascend", 1)
	createWeightedProject(t, state, "nvidia", 1)
	if _, err := state.RegisterNode(model.Node{ID: "nvidia-node", GPUCount: 1, VRAMGB: 24, Labels: map[string]string{
		model.LabelAcceleratorVendor: model.VendorNVIDIA, model.LabelAcceleratorRuntime: model.RuntimeCUDA,
	}}); err != nil {
		t.Fatal(err)
	}
	ascendJob, err := state.CreateJobForProject("ascend", model.JobCreate{Name: "ascend", Image: "work", Requirements: model.Requirements{GPUCount: 1, Labels: map[string]string{
		model.LabelAcceleratorVendor: model.VendorHuawei, model.LabelAcceleratorRuntime: model.RuntimeCANN,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	nvidiaJob, err := state.CreateJobForProject("nvidia", model.JobCreate{Name: "nvidia", Image: "work", Requirements: model.Requirements{GPUCount: 1, Labels: map[string]string{
		model.LabelAcceleratorVendor: model.VendorNVIDIA, model.LabelAcceleratorRuntime: model.RuntimeCUDA,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	blocked, _ := state.GetJob(ascendJob.ID)
	scheduled, _ := state.GetJob(nvidiaJob.ID)
	ascendProject, _ := state.GetProject("ascend")
	nvidiaProject, _ := state.GetProject("nvidia")
	if blocked.Status != model.JobQueued || scheduled.Status != model.JobAssigned {
		t.Fatalf("blocked vendor stopped runnable work: blocked=%+v scheduled=%+v", blocked, scheduled)
	}
	if ascendProject.SchedulerVRuntime != 0 || nvidiaProject.SchedulerVRuntime != fairSchedulingStride {
		t.Fatalf("blocked vendor was charged: ascend=%d nvidia=%d", ascendProject.SchedulerVRuntime, nvidiaProject.SchedulerVRuntime)
	}
}

func TestFairSchedulerBlockedProjectCannotResetAReenteringProject(t *testing.T) {
	state := NewMemory()
	state.SetProjectFairScheduling(true)
	state.SetHeterogeneousAccelerators(true)
	createWeightedProject(t, state, "pin", 1)
	createWeightedProject(t, state, "a-steady", 1)
	createWeightedProject(t, state, "z-burst", 1)
	if _, err := state.RegisterNode(model.Node{ID: "nvidia-node", GPUCount: 1, VRAMGB: 24, Labels: map[string]string{
		model.LabelAcceleratorVendor: model.VendorNVIDIA, model.LabelAcceleratorRuntime: model.RuntimeCUDA,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.CreateJobForProject("pin", model.JobCreate{Name: "blocked", Image: "work", Requirements: model.Requirements{GPUCount: 1, Labels: map[string]string{
		model.LabelAcceleratorVendor: model.VendorHuawei, model.LabelAcceleratorRuntime: model.RuntimeCANN,
	}}}); err != nil {
		t.Fatal(err)
	}
	state.state.Projects["a-steady"].SchedulerVRuntime = 7 * fairSchedulingStride
	queueFairJobs(t, state, "a-steady", "steady", 1, 1, 0)
	state.state.Projects["z-burst"].SchedulerVRuntime = 9 * fairSchedulingStride
	queueFairJobs(t, state, "z-burst", "burst", 1, 1, 0)

	burst, _ := state.GetProject("z-burst")
	if burst.SchedulerVRuntime != 9*fairSchedulingStride {
		t.Fatalf("reentry moved vruntime backwards through a blocked project: got %d", burst.SchedulerVRuntime)
	}
	assigned := scheduleAndCompleteOne(t, state)
	if assigned.ProjectID != "a-steady" {
		t.Fatalf("blocked project let burst traffic starve steady traffic: assigned %s", assigned.ProjectID)
	}
}

func TestFairSchedulerSkipsUnmatchableHigherPriorityInsideProject(t *testing.T) {
	state := NewMemory()
	state.SetProjectFairScheduling(true)
	state.SetHeterogeneousAccelerators(true)
	createWeightedProject(t, state, "alpha", 1)
	if _, err := state.RegisterNode(model.Node{ID: "nvidia-node", GPUCount: 1, VRAMGB: 24, Labels: map[string]string{
		model.LabelAcceleratorVendor: model.VendorNVIDIA, model.LabelAcceleratorRuntime: model.RuntimeCUDA,
	}}); err != nil {
		t.Fatal(err)
	}
	blocked, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "blocked-high", Image: "work", Priority: 100, Requirements: model.Requirements{GPUCount: 1, Labels: map[string]string{
		model.LabelAcceleratorVendor: model.VendorHuawei, model.LabelAcceleratorRuntime: model.RuntimeCANN,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	runnable, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "runnable-low", Image: "work", Priority: 1, Requirements: model.Requirements{GPUCount: 1, Labels: map[string]string{
		model.LabelAcceleratorVendor: model.VendorNVIDIA, model.LabelAcceleratorRuntime: model.RuntimeCUDA,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	blockedAfter, _ := state.GetJob(blocked.ID)
	runnableAfter, _ := state.GetJob(runnable.ID)
	if blockedAfter.Status != model.JobQueued || runnableAfter.Status != model.JobAssigned {
		t.Fatalf("unmatchable high-priority job blocked its project queue: high=%+v low=%+v", blockedAfter, runnableAfter)
	}
}

func TestFairSchedulerRechecksHardQuotaAfterEveryAssignment(t *testing.T) {
	state := NewMemory()
	state.SetProjectFairScheduling(true)
	if _, err := state.CreateProject(model.ProjectCreate{
		ID: "limited", Name: "limited", Weight: 1, MaxConcurrentJobs: 1, MaxGPUs: 1,
	}); err != nil {
		t.Fatal(err)
	}
	createWeightedProject(t, state, "other", 1)
	for _, nodeID := range []string{"worker-a", "worker-b"} {
		if _, err := state.RegisterNode(model.Node{ID: nodeID, GPUCount: 1, VRAMGB: 24}); err != nil {
			t.Fatal(err)
		}
	}
	queueFairJobs(t, state, "limited", "limited", 2, 1, MaxJobPriority)
	queueFairJobs(t, state, "other", "other", 1, 1, MinJobPriority)

	if err := state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	assignedByProject := map[string]int{}
	queuedByProject := map[string]int{}
	for _, job := range state.ListJobs() {
		switch job.Status {
		case model.JobAssigned:
			assignedByProject[job.ProjectID]++
		case model.JobQueued:
			queuedByProject[job.ProjectID]++
		}
	}
	if assignedByProject["limited"] != 1 || queuedByProject["limited"] != 1 || assignedByProject["other"] != 1 {
		t.Fatalf("fair scheduling bypassed a hard quota or blocked another project: assigned=%+v queued=%+v", assignedByProject, queuedByProject)
	}
}

func TestFairSchedulerRollsBackAssignmentAndVRuntimeTogether(t *testing.T) {
	state := NewMemory()
	state.SetProjectFairScheduling(true)
	createWeightedProject(t, state, "alpha", 1)
	if _, err := state.RegisterNode(model.Node{ID: "worker", GPUCount: 1, VRAMGB: 24}); err != nil {
		t.Fatal(err)
	}
	job, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "job", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", "root@tcp(127.0.0.1:1)/gpuflow")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	state.db = db

	if err := state.Schedule(time.Minute); !errors.Is(err, ErrPersistence) {
		t.Fatalf("expected persistence failure, got %v", err)
	}
	rolledBackJob, _ := state.GetJob(job.ID)
	rolledBackProject, _ := state.GetProject("alpha")
	nodes := state.ListNodes()
	if rolledBackJob.Status != model.JobQueued || rolledBackProject.SchedulerVRuntime != 0 || len(nodes) != 1 || nodes[0].Busy {
		t.Fatalf("assignment and vruntime did not roll back together: job=%+v project=%+v nodes=%+v", rolledBackJob, rolledBackProject, nodes)
	}
}

func TestFairSchedulingDefaultsValidationRerunAndReentry(t *testing.T) {
	state := NewMemory()
	state.SetProjectFairScheduling(true)
	defaultProject, err := state.GetProject("default")
	if err != nil || defaultProject.Weight != DefaultProjectWeight {
		t.Fatalf("unexpected default weight: %+v err=%v", defaultProject, err)
	}
	alpha := createWeightedProject(t, state, "alpha", 0)
	if alpha.Weight != DefaultProjectWeight {
		t.Fatalf("zero create weight was not normalized: %+v", alpha)
	}
	if _, err := state.CreateProject(model.ProjectCreate{ID: "invalid", Name: "invalid", Weight: MaxProjectWeight + 1}); !errors.Is(err, ErrInvalidProject) {
		t.Fatalf("invalid create weight was accepted: %v", err)
	}
	zero := 0
	if _, err := state.UpdateProject("alpha", model.ProjectUpdate{Weight: &zero}); !errors.Is(err, ErrInvalidProject) {
		t.Fatalf("zero update weight was accepted: %v", err)
	}
	for _, priority := range []int{MinJobPriority - 1, MaxJobPriority + 1} {
		if _, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "invalid-priority", Image: "work", Priority: priority}); !errors.Is(err, ErrInvalidResources) {
			t.Fatalf("priority %d was accepted: %v", priority, err)
		}
	}

	first, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "first", Image: "work", Priority: 73})
	if err != nil {
		t.Fatal(err)
	}
	state.state.Projects["alpha"].SchedulerVRuntime = 7 * fairSchedulingStride
	if _, err := state.RegisterNode(model.Node{ID: "worker", GPUCount: 1, VRAMGB: 24}); err != nil {
		t.Fatal(err)
	}
	beta := createWeightedProject(t, state, "beta", 1)
	if _, err := state.CreateJobForProject("beta", model.JobCreate{Name: "beta", Image: "work"}); err != nil {
		t.Fatal(err)
	}
	beta, _ = state.GetProject("beta")
	if beta.SchedulerVRuntime != 7*fairSchedulingStride {
		t.Fatalf("re-entering project did not align to active minimum: %+v", beta)
	}
	if _, err := state.CancelJob(first.ID); err != nil {
		t.Fatal(err)
	}
	rerun, err := state.RerunJob(first.ID)
	if err != nil || rerun.Priority != first.Priority {
		t.Fatalf("rerun lost priority: %+v err=%v", rerun, err)
	}
}

func TestFairSchedulingOffPreservesFIFOAndNodeTiesAreStable(t *testing.T) {
	state := NewMemory()
	createWeightedProject(t, state, "alpha", 1)
	first, err := state.CreateJob(model.JobCreate{Name: "first", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = state.CreateJobForProject("alpha", model.JobCreate{Name: "second", Image: "work", Priority: MaxJobPriority, Requirements: model.Requirements{GPUCount: 1}})
	for _, nodeID := range []string{"node-b", "node-a"} {
		if _, err := state.RegisterNode(model.Node{ID: nodeID, GPUCount: 1, VRAMGB: 24, HourlyPrice: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _ := state.GetJob(first.ID)
	if got.Status != model.JobAssigned || got.AssignedNode != "node-a" {
		t.Fatalf("legacy FIFO or stable node tie was lost: %+v", got)
	}
}
