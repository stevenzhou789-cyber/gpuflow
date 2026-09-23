package store

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"gpuflow/internal/model"
)

func TestHostResourceValidation(t *testing.T) {
	for name, requirements := range map[string]model.Requirements{
		"negative CPU": {CPUCores: -1}, "NaN CPU": {CPUCores: math.NaN()},
		"infinite CPU": {CPUCores: math.Inf(1)}, "excessive CPU": {CPUCores: model.MaxCPUCores + 1},
		"CPU precision": {CPUCores: 0.0001}, "fractional millicore": {CPUCores: 1.0001},
		"negative memory": {MemoryMiB: -1}, "small memory": {MemoryMiB: 5},
		"excessive memory": {MemoryMiB: model.MaxMemoryMiB + 1},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewMemory().CreateJob(model.JobCreate{Name: name, Image: "work", Requirements: requirements})
			if !errors.Is(err, ErrInvalidResources) {
				t.Fatalf("invalid request accepted: %v", err)
			}
		})
	}
}

func TestHostReservationsLimitConcurrentJobsAndReleaseAfterCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cpu    float64
		memory int64
	}{
		{"CPU", 2, 1024}, {"memory", 0.5, 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewMemory()
			s.SetGPUGranularScheduling(true)
			_, err := s.RegisterNode(model.Node{ID: "host", GPUCount: 4, CPUCores: 4, MemoryMiB: 4096, HostResourceLimits: true})
			if err != nil {
				t.Fatal(err)
			}
			jobs := make([]*model.Job, 3)
			for i := range jobs {
				jobs[i], err = s.CreateJob(model.JobCreate{Image: "work", Requirements: model.Requirements{GPUCount: 1, CPUCores: tc.cpu, MemoryMiB: tc.memory}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Schedule(time.Minute); err != nil {
				t.Fatal(err)
			}
			for i, job := range jobs {
				stored, _ := s.GetJob(job.ID)
				want := model.JobAssigned
				if i == 2 {
					want = model.JobQueued
				}
				if stored.Status != want {
					t.Fatalf("job %d: %s, want %s", i, stored.Status, want)
				}
			}
			node := s.ListNodes()[0]
			if node.AllocatedCPUCores != tc.cpu*2 || node.AllocatedMemoryMiB != tc.memory*2 {
				t.Fatalf("reservation totals: %+v", node)
			}
			if _, err := s.UpdateJob(jobs[0].ID, "host", model.JobUpdate{Status: model.JobRunning}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CancelJob(jobs[0].ID); err != nil {
				t.Fatal(err)
			}
			if err := s.Schedule(time.Minute); err != nil {
				t.Fatal(err)
			}
			third, _ := s.GetJob(jobs[2].ID)
			if third.Status != model.JobQueued {
				t.Fatal("canceling job released host resources before confirmation")
			}
			if _, err := s.UpdateJob(jobs[0].ID, "host", model.JobUpdate{Status: model.JobCanceled}); err != nil {
				t.Fatal(err)
			}
			if err := s.Schedule(time.Minute); err != nil {
				t.Fatal(err)
			}
			third, _ = s.GetJob(jobs[2].ID)
			if third.Status != model.JobAssigned {
				t.Fatal("host resources not released after confirmed cancellation")
			}
		})
	}
}

func TestHostResourcesRequireKnownCapacityAndPreserveUnsetCompatibility(t *testing.T) {
	for name, r := range map[string]model.Requirements{
		"unknown CPU":    {GPUCount: 1, CPUCores: 1},
		"unknown memory": {GPUCount: 1, MemoryMiB: 1024},
		"legacy":         {GPUCount: 1},
	} {
		t.Run(name, func(t *testing.T) {
			s := NewMemory()
			s.SetGPUGranularScheduling(true)
			if _, err := s.RegisterNode(model.Node{ID: "old-agent", GPUCount: 2, HostResourceLimits: true}); err != nil {
				t.Fatal(err)
			}
			job, err := s.CreateJob(model.JobCreate{Image: "work", Requirements: r})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Schedule(time.Minute); err != nil {
				t.Fatal(err)
			}
			got, _ := s.GetJob(job.ID)
			want := model.JobQueued
			if name == "legacy" {
				want = model.JobAssigned
			}
			if got.Status != want {
				t.Fatalf("got %s, want %s", got.Status, want)
			}
		})
	}
}

func TestHostCapacityPersistenceAndHealthChanges(t *testing.T) {
	node := &model.Node{CPUCores: 8, MemoryMiB: 32768, HostResourceLimits: true}
	encoded, err := json.Marshal(detailsFromNode(node))
	if err != nil {
		t.Fatal(err)
	}
	var details nodeDetails
	if err := json.Unmarshal(encoded, &details); err != nil {
		t.Fatal(err)
	}
	var restored model.Node
	details.apply(&restored)
	if restored.MemoryMiB != node.MemoryMiB || !restored.HostResourceLimits {
		t.Fatal("memory capacity lost in MySQL details")
	}
	job := &model.Job{Requirements: model.Requirements{CPUCores: 1.5, MemoryMiB: 4096}}
	encoded, err = encodeJobRequirements(job)
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct{ model.Requirements }
	if err := json.Unmarshal(encoded, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.CPUCores != 1.5 || persisted.MemoryMiB != 4096 {
		t.Fatal("resource limits lost in job persistence")
	}

	s := NewMemory()
	if _, err := s.RegisterNode(model.Node{ID: "host", CPUCores: 4, MemoryMiB: 4096, HostResourceLimits: true}); err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateJob(model.JobCreate{Image: "work", Requirements: model.Requirements{CPUCores: 4, MemoryMiB: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	// Old Agents omit these fields; absence must not erase known capacity.
	oldUpdate := model.NodeHealthUpdate{Status: "HEALTHY"}
	updated, err := s.UpdateNodeHealth("host", oldUpdate)
	if err != nil || updated.MemoryMiB != 4096 || updated.CPUCores != 4 {
		t.Fatalf("old update erased host capacity: %+v %v", updated, err)
	}
	unknown := int64(0)
	oldUpdate.MemoryMiB = &unknown
	if _, err := s.UpdateNodeHealth("host", oldUpdate); err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(created.ID)
	if got.Status != model.JobQueued {
		t.Fatalf("capacity loss did not invalidate assignment: %s", got.Status)
	}
}

func TestLegacyAgentCannotReceiveLimitsItDoesNotEnforce(t *testing.T) {
	s := NewMemory()
	if _, err := s.RegisterNode(model.Node{ID: "old-agent", CPUCores: 8, MemoryMiB: 16384}); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateJob(model.JobCreate{Image: "work", Requirements: model.Requirements{CPUCores: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(job.ID)
	if got.Status != model.JobQueued {
		t.Fatal("CPU request dispatched to an Agent that never declared enforcement support")
	}
}

func TestFailedResultCanDisableAutomaticRecompute(t *testing.T) {
	no, yes := false, true
	for name, retryable := range map[string]*bool{"default": nil, "allowed": &yes, "disabled": &no} {
		t.Run(name, func(t *testing.T) {
			s := NewMemory()
			if _, err := s.RegisterNode(model.Node{ID: "host"}); err != nil {
				t.Fatal(err)
			}
			job, err := s.CreateJob(model.JobCreate{Image: "work", MaxRetries: 3})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Schedule(time.Minute); err != nil {
				t.Fatal(err)
			}
			if _, err := s.UpdateJob(job.ID, "host", model.JobUpdate{Status: model.JobRunning}); err != nil {
				t.Fatal(err)
			}
			got, err := s.UpdateJob(job.ID, "host", model.JobUpdate{Status: model.JobFailed, Retryable: retryable, Error: "artifact delivery failed"})
			if err != nil {
				t.Fatal(err)
			}
			want := model.JobQueued
			if name == "disabled" {
				want = model.JobFailed
			}
			if got.Status != want {
				t.Fatalf("got %s, want %s", got.Status, want)
			}
		})
	}
}
