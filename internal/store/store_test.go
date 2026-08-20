package store

import (
	"errors"
	"reflect"
	"strings"
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
	if recovered == nil || recovered.ID != job.ID || recovered.Status != model.JobAssigned {
		t.Fatalf("expected interrupted job to be reassigned, got %+v", recovered)
	}
	if recovered.StartedAt != nil || recovered.Output != "" || recovered.Attempts != 2 || recovered.Recoveries != 1 {
		t.Fatalf("unexpected recovered attempt: %+v", recovered)
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
	if exhausted.Status != model.JobFailed || exhausted.Recoveries != 1 || exhaustedNode.Busy || exhaustedNode.CurrentJob != "" {
		t.Fatalf("expected exhausted recovery budget to fail and release the node: job=%+v node=%+v", exhausted, exhaustedNode)
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
	if !reflect.DeepEqual(first.AllocatedGPUs, []int{0, 1}) || !reflect.DeepEqual(second.AllocatedGPUs, []int{2, 3}) {
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
	if first.Status != model.JobAssigned || second.Status != model.JobQueued || len(first.AllocatedGPUs) != 0 {
		t.Fatalf("community scheduling stopped being whole-node exclusive: %+v %+v", first, second)
	}
}
