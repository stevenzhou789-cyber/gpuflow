package store

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"gpuflow/internal/model"
	"gpuflow/pkg/projectscope"
)

func intPointer(value int) *int { return &value }

func statusPointer(value model.ProjectStatus) *model.ProjectStatus { return &value }

func createTestProject(t *testing.T, state *Store, id string, queued, concurrent, gpus int) *model.Project {
	t.Helper()
	project, err := state.CreateProject(model.ProjectCreate{
		ID: id, Name: id, MaxQueuedJobs: queued, MaxConcurrentJobs: concurrent, MaxGPUs: gpus,
	})
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func requireQuotaCode(t *testing.T, err error, code string) {
	t.Helper()
	if !errors.Is(err, ErrProjectQuota) {
		t.Fatalf("expected project quota error, got %v", err)
	}
	var quota *ProjectQuotaError
	if !errors.As(err, &quota) || quota.Code != code {
		t.Fatalf("expected quota code %q, got %+v (%v)", code, quota, err)
	}
}

func TestDefaultProjectPreservesCommunityBehavior(t *testing.T) {
	state := NewMemory()
	project, err := state.GetProject("")
	if err != nil || project.ID != projectscope.DefaultProjectID || project.Status != model.ProjectActive || project.MaxQueuedJobs != 0 || project.MaxConcurrentJobs != 0 || project.MaxGPUs != 0 {
		t.Fatalf("unexpected default project: %+v err=%v", project, err)
	}
	if _, exists := reflect.TypeOf(model.JobCreate{}).FieldByName("ProjectID"); exists {
		t.Fatal("client-controlled JobCreate exposes ProjectID")
	}
	for index := 0; index < 3; index++ {
		job, err := state.CreateJob(model.JobCreate{Name: "community", Image: "alpine"})
		if err != nil || job.ProjectID != projectscope.DefaultProjectID {
			t.Fatalf("default-project create failed: %+v err=%v", job, err)
		}
	}
	if _, err := state.UpdateProject(projectscope.DefaultProjectID, model.ProjectUpdate{Status: statusPointer(model.ProjectDisabled)}); !errors.Is(err, ErrProjectConflict) {
		t.Fatalf("default project was disabled: %v", err)
	}
}

func TestProjectQueueQuotaIsAtomicAndRerunInheritsProject(t *testing.T) {
	state := NewMemory()
	createTestProject(t, state, "alpha", 1, 0, 0)

	var wait sync.WaitGroup
	errorsByCall := make(chan error, 2)
	jobs := make(chan *model.Job, 2)
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			job, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "queued", Image: "alpine"})
			jobs <- job
			errorsByCall <- err
		}()
	}
	wait.Wait()
	close(jobs)
	close(errorsByCall)
	succeeded, rejected := 0, 0
	var original *model.Job
	for job := range jobs {
		if job != nil {
			succeeded++
			original = job
		}
	}
	for err := range errorsByCall {
		if err != nil {
			requireQuotaCode(t, err, ProjectQuotaQueue)
			rejected++
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("queue quota was bypassed: succeeded=%d rejected=%d", succeeded, rejected)
	}
	if _, err := state.CancelJobForScope(projectscope.Project("alpha"), original.ID); err != nil {
		t.Fatal(err)
	}
	rerun, err := state.RerunJobForScope(projectscope.Project("alpha"), original.ID)
	if err != nil || rerun.ProjectID != "alpha" || rerun.RerunOf != original.ID {
		t.Fatalf("rerun lost project identity: %+v err=%v", rerun, err)
	}
	if _, err := state.RerunJobForScope(projectscope.Project("alpha"), original.ID); err == nil {
		t.Fatal("rerun bypassed the full project queue")
	} else {
		requireQuotaCode(t, err, ProjectQuotaQueue)
	}
	if _, err := state.RerunJobForScope(projectscope.Project("beta"), original.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project rerun did not return not found: %v", err)
	}
}

func TestProjectScopeFiltersEveryTaskMutation(t *testing.T) {
	state := NewMemory()
	createTestProject(t, state, "alpha", 0, 0, 0)
	createTestProject(t, state, "beta", 0, 0, 0)
	alpha, _ := state.CreateJobForProject("alpha", model.JobCreate{Name: "alpha", Image: "alpine"})
	beta, _ := state.CreateJobForProject("beta", model.JobCreate{Name: "beta", Image: "alpine"})
	alphaScope := projectscope.Project("alpha")

	if _, err := state.GetJobForScope(alphaScope, beta.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project get leaked existence: %v", err)
	}
	if jobs := state.ListJobsForScope(alphaScope); len(jobs) != 1 || jobs[0].ID != alpha.ID {
		t.Fatalf("scoped list leaked jobs: %+v", jobs)
	}
	if page := state.QueryJobsForScope(alphaScope, JobQuery{}); page.Total != 1 || page.Items[0].ID != alpha.ID {
		t.Fatalf("scoped query leaked jobs: %+v", page)
	}
	if _, err := state.CancelJobForScope(alphaScope, beta.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project cancel leaked existence: %v", err)
	}
	if err := state.ValidateJobDeletionForScope(alphaScope, beta.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project deletion validation leaked existence: %v", err)
	}
	if _, err := state.CancelJobForScope(projectscope.Project("beta"), beta.ID); err != nil {
		t.Fatal(err)
	}
	if err := state.BeginJobDeletionForScope(alphaScope, beta.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project deletion marker leaked existence: %v", err)
	}
	if err := state.DeleteJobForScope(alphaScope, beta.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project deletion leaked existence: %v", err)
	}
}

func TestProjectSchedulingEnforcesConcurrentAndGPUQuotas(t *testing.T) {
	state := NewMemory()
	state.SetGPUGranularScheduling(true)
	createTestProject(t, state, "alpha", 4, 1, 2)
	if _, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "too large", Image: "work", Requirements: model.Requirements{GPUCount: 3}}); err == nil {
		t.Fatal("single job larger than project GPU quota was accepted")
	} else {
		requireQuotaCode(t, err, ProjectQuotaGPU)
	}
	for _, id := range []string{"node-a", "node-b"} {
		if _, err := state.RegisterNode(model.Node{ID: id, GPUModel: "gpu", GPUCount: 2, VRAMGB: 16}); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 2; index++ {
		if _, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "gpu", Image: "work", Requirements: model.Requirements{GPUCount: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	snapshot, err := state.ProjectQuotaSnapshot("alpha")
	if err != nil || snapshot.ConcurrentJobs != 1 || snapshot.AllocatedGPUs != 2 || snapshot.QueuedJobs != 1 {
		t.Fatalf("unexpected hard-quota snapshot: %+v err=%v", snapshot, err)
	}
	if _, err := state.UpdateProject("alpha", model.ProjectUpdate{MaxConcurrentJobs: intPointer(0), MaxGPUs: intPointer(1)}); !errors.Is(err, ErrProjectConflict) {
		t.Fatalf("quota was lowered below active allocation: %v", err)
	}

	gpuLimited := NewMemory()
	gpuLimited.SetGPUGranularScheduling(true)
	createTestProject(t, gpuLimited, "gpu-limit", 4, 0, 2)
	for _, id := range []string{"gpu-node-a", "gpu-node-b"} {
		_, _ = gpuLimited.RegisterNode(model.Node{ID: id, GPUModel: "gpu", GPUCount: 2, VRAMGB: 16})
		_, _ = gpuLimited.CreateJobForProject("gpu-limit", model.JobCreate{Name: id, Image: "work", Requirements: model.Requirements{GPUCount: 2}})
	}
	_ = gpuLimited.Schedule(time.Minute)
	gpuSnapshot, _ := gpuLimited.ProjectQuotaSnapshot("gpu-limit")
	if gpuSnapshot.ConcurrentJobs != 1 || gpuSnapshot.AllocatedGPUs != 2 || gpuSnapshot.QueuedJobs != 1 {
		t.Fatalf("aggregate GPU quota was not enforced independently: %+v", gpuSnapshot)
	}

	concurrentLimited := NewMemory()
	createTestProject(t, concurrentLimited, "concurrent-limit", 4, 1, 0)
	for _, id := range []string{"cpu-node-a", "cpu-node-b"} {
		_, _ = concurrentLimited.RegisterNode(model.Node{ID: id})
		_, _ = concurrentLimited.CreateJobForProject("concurrent-limit", model.JobCreate{Name: id, Image: "work"})
	}
	_ = concurrentLimited.Schedule(time.Minute)
	concurrentSnapshot, _ := concurrentLimited.ProjectQuotaSnapshot("concurrent-limit")
	if concurrentSnapshot.ConcurrentJobs != 1 || concurrentSnapshot.AllocatedGPUs != 0 || concurrentSnapshot.QueuedJobs != 1 {
		t.Fatalf("concurrency quota was not enforced independently: %+v", concurrentSnapshot)
	}
}

func TestProjectQuotaCannotBeLoweredBelowCurrentQueueOrConcurrency(t *testing.T) {
	queued := NewMemory()
	createTestProject(t, queued, "queue", 2, 0, 0)
	for index := 0; index < 2; index++ {
		_, _ = queued.CreateJobForProject("queue", model.JobCreate{Name: "queued", Image: "work"})
	}
	if _, err := queued.UpdateProject("queue", model.ProjectUpdate{MaxQueuedJobs: intPointer(1)}); !errors.Is(err, ErrProjectConflict) {
		t.Fatalf("queue quota was lowered below current usage: %v", err)
	}

	concurrent := NewMemory()
	concurrent.SetGPUGranularScheduling(true)
	createTestProject(t, concurrent, "running", 2, 2, 0)
	for _, id := range []string{"node-a", "node-b"} {
		_, _ = concurrent.RegisterNode(model.Node{ID: id, GPUModel: "gpu", GPUCount: 1, VRAMGB: 8})
		_, _ = concurrent.CreateJobForProject("running", model.JobCreate{Name: id, Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	}
	_ = concurrent.Schedule(time.Minute)
	snapshot, _ := concurrent.ProjectQuotaSnapshot("running")
	if snapshot.ConcurrentJobs != 2 {
		t.Fatalf("test setup did not create two active jobs: %+v", snapshot)
	}
	if _, err := concurrent.UpdateProject("running", model.ProjectUpdate{MaxConcurrentJobs: intPointer(1)}); !errors.Is(err, ErrProjectConflict) {
		t.Fatalf("concurrency quota was lowered below current usage: %v", err)
	}
}

func TestDisabledProjectPausesExistingQueueAndRejectsNewJobs(t *testing.T) {
	state := NewMemory()
	createTestProject(t, state, "alpha", 2, 2, 2)
	job, _ := state.CreateJobForProject("alpha", model.JobCreate{Name: "waiting", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	if _, err := state.UpdateProject("alpha", model.ProjectUpdate{Status: statusPointer(model.ProjectDisabled)}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "new", Image: "work"}); !errors.Is(err, ErrProjectDisabled) {
		t.Fatalf("disabled project accepted a new job: %v", err)
	}
	if _, err := state.RerunJobForScope(projectscope.Project("alpha"), job.ID); !errors.Is(err, ErrProjectDisabled) {
		t.Fatalf("disabled project accepted a rerun: %v", err)
	}
	_, _ = state.RegisterNode(model.Node{ID: "node", GPUModel: "gpu", GPUCount: 1, VRAMGB: 8})
	if err := state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	stored, _ := state.GetJob(job.ID)
	if stored.Status != model.JobQueued {
		t.Fatalf("disabled project queue was scheduled: %+v", stored)
	}
}

func TestSystemRequeueFailsWhenProjectQueueIsFull(t *testing.T) {
	state := NewMemory()
	createTestProject(t, state, "alpha", 1, 0, 0)
	_, _ = state.RegisterNode(model.Node{ID: "node", GPUModel: "gpu", GPUCount: 1, VRAMGB: 8})
	assigned, _ := state.CreateJobForProject("alpha", model.JobCreate{Name: "assigned", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	if err := state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "fills queue", Image: "work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.UpdateNodeHealth("node", model.NodeHealthUpdate{Status: "DEGRADED"}); err != nil {
		t.Fatal(err)
	}
	failed, _ := state.GetJob(assigned.ID)
	if failed.Status != model.JobFailed || failed.Error != ProjectQuotaQueue || failed.FinishedAt == nil {
		t.Fatalf("system requeue bypassed full queue: %+v", failed)
	}
}

func TestFailedAttemptRetryCannotBypassQueueQuota(t *testing.T) {
	state := NewMemory()
	createTestProject(t, state, "alpha", 1, 0, 0)
	node, _ := state.RegisterNode(model.Node{ID: "node", GPUModel: "gpu", GPUCount: 1, VRAMGB: 8})
	running, _ := state.CreateJobForProject("alpha", model.JobCreate{Name: "running", Image: "work", MaxRetries: 1, Requirements: model.Requirements{GPUCount: 1}})
	_ = state.Schedule(time.Minute)
	_, _ = state.UpdateJob(running.ID, node.ID, model.JobUpdate{Status: model.JobRunning})
	if _, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "fills queue", Image: "work"}); err != nil {
		t.Fatal(err)
	}
	failed, err := state.UpdateJob(running.ID, node.ID, model.JobUpdate{Status: model.JobFailed, Error: "work failed"})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != model.JobFailed || failed.Error != ProjectQuotaQueue || failed.FinishedAt == nil {
		t.Fatalf("automatic retry bypassed full queue: %+v", failed)
	}
}

func TestRecoveredAttemptCannotBypassQueueQuota(t *testing.T) {
	state := NewMemory()
	createTestProject(t, state, "alpha", 1, 0, 0)
	node, _ := state.RegisterNode(model.Node{ID: "node", GPUModel: "gpu", GPUCount: 1, VRAMGB: 8})
	running, _ := state.CreateJobForProject("alpha", model.JobCreate{Name: "running", Image: "work", MaxRetries: 1, Requirements: model.Requirements{GPUCount: 1}})
	_ = state.Schedule(time.Minute)
	_, _ = state.NextJob(node.ID)
	_, _ = state.UpdateJob(running.ID, node.ID, model.JobUpdate{Status: model.JobRunning})
	if _, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "fills queue", Image: "work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.RegisterNode(model.Node{ID: node.ID, GPUModel: "gpu", GPUCount: 1, VRAMGB: 8}); err != nil {
		t.Fatal(err)
	}
	cleanup, err := state.NextJob(node.ID)
	if err != nil || cleanup == nil || cleanup.Status != model.JobCanceling {
		t.Fatalf("recovery cleanup was not dispatched: %+v err=%v", cleanup, err)
	}
	failed, err := state.UpdateJob(running.ID, node.ID, model.JobUpdate{Status: model.JobCanceled})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != model.JobFailed || failed.Error != ProjectQuotaQueue || failed.FinishedAt == nil {
		t.Fatalf("recovery requeue bypassed full queue: %+v", failed)
	}
}
