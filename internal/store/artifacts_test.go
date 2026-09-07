package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gpuflow/internal/model"
)

func TestArtifactReferencePublicationFencingPersistenceAndSnapshots(t *testing.T) {
	s := NewMemory()
	node, err := s.RegisterNodeSession(model.Node{ID: "artifact-node"}, "artifact-session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmNodeCleanupSession(node.ID, "artifact-session"); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateJob(model.JobCreate{Name: "artifact", Image: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	dispatch, err := s.NextJobSession(node.ID, "artifact-session")
	if err != nil || dispatch == nil {
		t.Fatalf("dispatch: %+v %v", dispatch, err)
	}
	if _, err := s.UpdateJobLease(job.ID, node.ID, "artifact-session", dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatal(err)
	}
	first := model.ArtifactReference{StorageID: job.ID + "/.gpuflow-versions/first", Size: 12, LastModified: time.Now().UTC().Truncate(time.Microsecond)}
	if err := s.PublishJobArtifact(job.ID, node.ID, "artifact-session", dispatch.AttemptToken, "training.log", first); err != nil {
		t.Fatal(err)
	}
	read, _ := s.GetJob(job.ID)
	encoded, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "artifact_refs") || strings.Contains(string(encoded), ".gpuflow-versions") {
		t.Fatalf("internal reference leaked through Job JSON: %s", encoded)
	}
	persisted, err := encodeJobRequirements(read)
	if err != nil {
		t.Fatal(err)
	}
	var restored model.Job
	if err := decodeJobJSON(&restored, []byte("null"), []byte("null"), persisted); err != nil {
		t.Fatal(err)
	}
	if restored.ArtifactRefs["training.log"] != first {
		t.Fatalf("reference lost during persistence: %s", persisted)
	}
	second := first
	second.StorageID = job.ID + "/.gpuflow-versions/second"
	if err := s.PublishJobArtifact(job.ID, node.ID, "artifact-session", "stale-attempt", "training.log", second); !errors.Is(err, ErrAttemptLease) {
		t.Fatalf("stale attempt publication: %v", err)
	}
	if err := s.PublishJobArtifact(job.ID, node.ID, "artifact-session", dispatch.AttemptToken, "training.log", second); err != nil {
		t.Fatal(err)
	}
	if read.ArtifactRefs["training.log"] != first {
		t.Fatal("publication mutated an earlier Job snapshot")
	}
	read.ArtifactRefs["training.log"] = model.ArtifactReference{StorageID: "wrong"}
	current, _ := s.GetJob(job.ID)
	if current.ArtifactRefs["training.log"] != second {
		t.Fatal("caller could mutate stored artifact references")
	}
	// Exercise the real transaction failure path without needing a live MySQL.
	db, err := sql.Open("mysql", "unused@tcp(127.0.0.1:1)/unused")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s.db = db
	if err := s.PublishJobArtifact(job.ID, node.ID, "artifact-session", dispatch.AttemptToken, "training.log", first); !errors.Is(err, ErrPersistence) {
		t.Fatalf("expected rollback, got %v", err)
	}
	s.db = nil
	current, _ = s.GetJob(job.ID)
	if current.ArtifactRefs["training.log"] != second {
		t.Fatal("failed publication replaced the visible reference")
	}
	if _, err := s.RegisterNode(model.Node{ID: node.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishJobArtifact(job.ID, node.ID, "artifact-session", dispatch.AttemptToken, "training.log", first); !errors.Is(err, ErrAgentSession) {
		t.Fatalf("stale session publication: %v", err)
	}
	current, _ = s.GetJob(job.ID)
	if current.ArtifactRefs["training.log"] != second {
		t.Fatal("stale publication replaced the visible reference")
	}
}

func TestArtifactReferenceAbsentInLegacyJobRequirements(t *testing.T) {
	var job model.Job
	if err := decodeJobJSON(&job, []byte("null"), []byte("null"), []byte(`{"gpu_count":1}`)); err != nil {
		t.Fatal(err)
	}
	if len(job.ArtifactRefs) != 0 || job.Requirements.GPUCount != 1 {
		t.Fatalf("legacy requirements changed: %+v", job)
	}
}
