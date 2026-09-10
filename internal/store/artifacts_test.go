package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	first.Attempt = 1
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

func startArtifactAttempt(t *testing.T, s *Store, node *model.Node, jobID string) *model.AgentJob {
	t.Helper()
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	dispatch, err := s.NextJobSession(node.ID, node.SessionEpoch)
	if err != nil || dispatch == nil || dispatch.Job.ID != jobID {
		t.Fatalf("dispatch: %+v %v", dispatch, err)
	}
	if _, err := s.UpdateJobLease(jobID, node.ID, node.SessionEpoch, dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatal(err)
	}
	return dispatch
}

func TestArtifactPublicationAttributesRetryAndRecoveryAttempts(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovery=%t", recovery), func(t *testing.T) {
			s := NewMemory()
			node, err := s.RegisterNode(model.Node{ID: "artifact-retry"})
			if err != nil {
				t.Fatal(err)
			}
			job, err := s.CreateJob(model.JobCreate{Name: "retry", Image: "work", MaxRetries: 1})
			if err != nil {
				t.Fatal(err)
			}
			first := startArtifactAttempt(t, s, node, job.ID)
			reference := model.ArtifactReference{StorageID: job.ID + "/.gpuflow-versions/first", Attempt: 999}
			if err := s.PublishJobArtifact(job.ID, node.ID, node.SessionEpoch, first.AttemptToken, "training.log", reference); err != nil {
				t.Fatal(err)
			}
			previous, _ := s.GetJob(job.ID)
			if previous.ArtifactRefs["training.log"].Attempt != 1 {
				t.Fatal("publication trusted the caller's attempt instead of the running job")
			}
			if recovery {
				node, err = s.RegisterNode(model.Node{ID: node.ID})
				if err != nil {
					t.Fatal(err)
				}
				cleanup, claimErr := s.NextJobSession(node.ID, node.SessionEpoch)
				if claimErr != nil || cleanup == nil || cleanup.Job.Status != model.JobCanceling {
					t.Fatalf("cleanup dispatch: %+v %v", cleanup, claimErr)
				}
				_, err = s.UpdateJobLease(job.ID, node.ID, node.SessionEpoch, cleanup.AttemptToken, model.JobUpdate{Status: model.JobCanceled})
			} else {
				_, err = s.UpdateJobLease(job.ID, node.ID, node.SessionEpoch, first.AttemptToken, model.JobUpdate{Status: model.JobFailed})
			}
			if err != nil {
				t.Fatal(err)
			}
			second := startArtifactAttempt(t, s, node, job.ID)
			current, _ := s.GetJob(job.ID)
			if current.Attempts != 2 || current.ArtifactRefs["training.log"].Attempt != 1 {
				t.Fatalf("retry relabeled previous output: %+v", current)
			}
			if err := s.PublishJobArtifact(job.ID, node.ID, node.SessionEpoch, first.AttemptToken, "training.log", reference); err == nil {
				t.Fatal("previous attempt was allowed to publish after retry")
			}
			reference.StorageID = job.ID + "/.gpuflow-versions/second"
			if err := s.PublishJobArtifact(job.ID, node.ID, node.SessionEpoch, second.AttemptToken, "training.log", reference); err != nil {
				t.Fatal(err)
			}
			current, _ = s.GetJob(job.ID)
			if current.ArtifactRefs["training.log"].Attempt != 2 || previous.ArtifactRefs["training.log"].Attempt != 1 {
				t.Fatal("current publication or earlier snapshot has incorrect attribution")
			}
			encoded, err := encodeJobRequirements(current)
			if err != nil {
				t.Fatal(err)
			}
			var restored model.Job
			if err := decodeJobJSON(&restored, []byte("null"), []byte("null"), encoded); err != nil {
				t.Fatal(err)
			}
			if restored.ArtifactRefs["training.log"] != current.ArtifactRefs["training.log"] {
				t.Fatal("attempt attribution was lost in persisted requirements")
			}
		})
	}
}

func TestLegacyArtifactReferenceDoesNotInventAttempt(t *testing.T) {
	var job model.Job
	if err := decodeJobJSON(&job, []byte("null"), []byte("null"), []byte(`{"artifact_refs":{"training.log":{"storage_id":"job/.gpuflow-versions/legacy","size":10}}}`)); err != nil {
		t.Fatal(err)
	}
	if reference, exists := job.ArtifactRefs["training.log"]; !exists || reference.Attempt != 0 {
		t.Fatalf("legacy attribution changed: %+v", job.ArtifactRefs)
	}
}

func TestMySQLArtifactAttemptAttributionSurvivesRetryAndReopen(t *testing.T) {
	dsn := os.Getenv("GPUFLOW_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("GPUFLOW_TEST_MYSQL_DSN is not set")
	}
	s, err := OpenMySQLStateStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	nodeID := fmt.Sprintf("artifact-mysql-%d", time.Now().UnixNano())
	labels := map[string]string{"artifact-test-node": nodeID}
	node, err := s.RegisterNode(model.Node{ID: nodeID, Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := s.db.Exec("DELETE FROM nodes WHERE id = ?", nodeID); err != nil {
			t.Errorf("clean up artifact test node: %v", err)
		}
	}()
	job, err := s.CreateJob(model.JobCreate{Name: "mysql artifact retry", Image: "work", MaxRetries: 1, Requirements: model.Requirements{Labels: labels}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := s.db.Exec("DELETE FROM jobs WHERE id = ?", job.ID); err != nil {
			t.Errorf("clean up artifact test job: %v", err)
		}
	}()
	first := startArtifactAttempt(t, s, node, job.ID)
	reference := model.ArtifactReference{StorageID: job.ID + "/.gpuflow-versions/first", Size: 3}
	if err := s.PublishJobArtifact(job.ID, node.ID, node.SessionEpoch, first.AttemptToken, "training.log", reference); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateJobLease(job.ID, node.ID, node.SessionEpoch, first.AttemptToken, model.JobUpdate{Status: model.JobFailed}); err != nil {
		t.Fatal(err)
	}
	second := startArtifactAttempt(t, s, node, job.ID)
	if _, err := s.UpdateJobLease(job.ID, node.ID, node.SessionEpoch, second.AttemptToken, model.JobUpdate{Status: model.JobSucceeded}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenMySQLStateStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	current, err := reopened.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != model.JobSucceeded || current.Attempts != 2 || current.ArtifactRefs["training.log"].Attempt != 1 {
		t.Fatalf("reopen lost the distinction between current job and previous output: %+v", current)
	}
}
