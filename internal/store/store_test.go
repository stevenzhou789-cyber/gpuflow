package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gpuflow/internal/model"
)

func TestScheduleChoosesCheapestEligibleNode(t *testing.T) {
	s, _ := Open("")
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

func TestTaskImagesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	image := model.TaskImage{ID: "img-test", Name: "gpuflow-task/test:v1", Status: "ready", CreatedAt: time.Now()}
	if err := s.SaveTaskImage(image); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	images, err := reopened.ListTaskImages()
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].Name != image.Name {
		t.Fatalf("unexpected persisted images: %+v", images)
	}
}

func TestFailedJobRetries(t *testing.T) {
	s, _ := Open("")
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

func TestDeleteNodeRejectsActiveJob(t *testing.T) {
	s, _ := Open("")
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
	s, _ := Open("")
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
