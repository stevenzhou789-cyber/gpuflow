package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuflow/internal/artifact"
	"gpuflow/internal/model"
	"gpuflow/internal/store"
	"gpuflow/pkg/edition"
)

func TestArtifactsFollowAutomaticRetryAttempt(t *testing.T) {
	for _, granular := range []bool{false, true} {
		descriptor := edition.Community()
		if granular {
			descriptor.Name = "enterprise-core"
			descriptor.Features[edition.FeatureGPUGranularScheduling] = true
		}
		t.Run(descriptor.Name, func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				failCopy string
				want     map[string]string
			}{
				{name: "no_uploads", want: map[string]string{}},
				{name: "artifact_upload_fails", failCopy: "artifacts.tar.gz", want: map[string]string{"training.log": "attempt two log"}},
				{name: "complete_log_upload_fails", failCopy: "training.log", want: map[string]string{"artifacts.tar.gz": "attempt two archive"}},
				{name: "both_replaced", want: map[string]string{"training.log": "attempt two log", "artifacts.tar.gz": "attempt two archive"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					backend := &publicationS3{objects: map[string][]byte{}, copyStarted: make(chan struct{}), releaseCopy: make(chan struct{})}
					close(backend.releaseCopy)
					s3 := httptest.NewServer(backend)
					defer s3.Close()
					artifacts, err := artifact.Open(artifact.Config{Endpoint: strings.TrimPrefix(s3.URL, "http://"), AccessKey: "test-access", SecretKey: "test-secret", Bucket: "artifacts", Region: "us-east-1"})
					if err != nil {
						t.Fatal(err)
					}
					state := store.NewMemory()
					server := httptest.NewServer(NewWithStores(state, state, artifacts, "test-token", descriptor).Handler())
					defer server.Close()
					const nodeID, session = "retry-node", "retry-session"
					if _, err = state.RegisterNodeSession(model.Node{ID: nodeID}, session); err != nil {
						t.Fatal(err)
					}
					if _, err = state.ConfirmNodeCleanupSession(nodeID, session); err != nil {
						t.Fatal(err)
					}
					job, err := state.CreateJob(model.JobCreate{Name: "artifact-retry", Image: "work", MaxRetries: 1})
					if err != nil {
						t.Fatal(err)
					}
					start := func(wantAttempt int) string {
						t.Helper()
						if err := state.Schedule(time.Minute); err != nil {
							t.Fatal(err)
						}
						dispatch, err := state.NextJobSession(nodeID, session)
						if err != nil || dispatch == nil || dispatch.ID != job.ID || dispatch.Attempts != wantAttempt {
							t.Fatalf("attempt %d dispatch: %+v %v", wantAttempt, dispatch, err)
						}
						if _, err = state.UpdateJobLease(job.ID, nodeID, session, dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
							t.Fatal(err)
						}
						return dispatch.AttemptToken
					}
					finish := func(token string, status model.JobStatus) {
						t.Helper()
						if _, err := state.UpdateJobLease(job.ID, nodeID, session, token, model.JobUpdate{Status: status, Output: "bounded output tail"}); err != nil {
							t.Fatal(err)
						}
					}
					upload := func(token, name, content string, wantStatus int) {
						t.Helper()
						response, err := http.DefaultClient.Do(publicationUploadFile(t, server, job.ID, nodeID, session, token, name, content))
						if err != nil {
							t.Fatal(err)
						}
						payload, _ := io.ReadAll(response.Body)
						response.Body.Close()
						if response.StatusCode != wantStatus {
							t.Fatalf("upload %s returned %d: %s", name, response.StatusCode, payload)
						}
					}
					token := start(1)
					// Canonical objects from an older installation must not resurface
					// when this job retries, even if no replacement is published.
					backend.mu.Lock()
					for _, name := range []string{"training.log", "artifacts.tar.gz", "legacy.txt"} {
						backend.objects[job.ID+"/"+name] = []byte("canonical " + name)
					}
					backend.mu.Unlock()
					first := map[string]string{"training.log": "attempt one log", "artifacts.tar.gz": "attempt one archive", "legacy.txt": "canonical legacy.txt"}
					for _, name := range []string{"training.log", "artifacts.tar.gz"} {
						upload(token, name, first[name], http.StatusCreated)
					}
					assertAttemptArtifacts(t, server, job.ID, first)
					backend.mu.Lock()
					historical := make(map[string][]byte, len(backend.objects))
					for name, payload := range backend.objects {
						historical[name] = append([]byte(nil), payload...)
					}
					backend.mu.Unlock()
					finish(token, model.JobFailed)
					token = start(2)
					assertAttemptArtifacts(t, server, job.ID, nil)
					backend.mu.Lock()
					backend.failCopyName = tc.failCopy
					backend.mu.Unlock()
					for _, name := range []string{"training.log", "artifacts.tar.gz"} {
						if name == tc.failCopy {
							upload(token, name, "failed replacement", http.StatusBadGateway)
						} else if content, ok := tc.want[name]; ok {
							upload(token, name, content, http.StatusCreated)
						}
					}
					finish(token, model.JobSucceeded)
					assertAttemptArtifacts(t, server, job.ID, tc.want)
					backend.mu.Lock()
					for name, payload := range historical {
						if !bytes.Equal(backend.objects[name], payload) {
							t.Errorf("historical object %s changed or disappeared before job deletion", name)
						}
					}
					backend.mu.Unlock()
					if status := request(t, server, http.MethodDelete, "/v1/jobs/"+job.ID, nil, nil); status != http.StatusNoContent {
						t.Fatalf("delete returned %d", status)
					}
					backend.mu.Lock()
					defer backend.mu.Unlock()
					for name := range backend.objects {
						if strings.HasPrefix(name, job.ID+"/") {
							t.Errorf("job deletion retained object %s", name)
						}
					}
				})
			}
		})
	}
}

func assertAttemptArtifacts(t *testing.T, server *httptest.Server, jobID string, want map[string]string) {
	t.Helper()
	var listing struct {
		Items []artifact.Item `json:"items"`
	}
	base := "/v1/jobs/" + jobID
	if status := request(t, server, http.MethodGet, base+"/artifacts", nil, &listing); status != http.StatusOK {
		t.Fatalf("list artifacts returned %d", status)
	}
	if len(listing.Items) != len(want) {
		t.Fatalf("artifact list = %+v, want %v", listing.Items, want)
	}
	for _, item := range listing.Items {
		if content, ok := want[item.Name]; !ok || item.Size != int64(len(content)) {
			t.Errorf("unexpected artifact listing entry %+v", item)
		}
	}
	for endpoint, name := range map[string]string{
		"/artifacts/training.log":     "training.log",
		"/artifacts/artifacts.tar.gz": "artifacts.tar.gz",
		"/artifacts/legacy.txt":       "legacy.txt",
		"/logs/full":                  "training.log",
	} {
		req, err := http.NewRequest(http.MethodGet, server.URL+base+endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer test-token")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := io.ReadAll(response.Body)
		response.Body.Close()
		content, exists := want[name]
		if !exists {
			if response.StatusCode != http.StatusNotFound {
				t.Errorf("%s returned %d %q, want 404", endpoint, response.StatusCode, payload)
			}
		} else if response.StatusCode != http.StatusOK || string(payload) != content {
			t.Errorf("%s returned %d %q, want 200 %q", endpoint, response.StatusCode, payload, content)
		}
	}
}

func TestArtifactStorageIDLegacyCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name             string
		attempts         int
		referenceAttempt int
		hasReference     bool
		wantVisible      bool
	}{
		{name: "legacy_canonical_before_dispatch", wantVisible: true},
		{name: "legacy_canonical_first_attempt", attempts: 1, wantVisible: true},
		{name: "legacy_canonical_after_retry", attempts: 2},
		{name: "legacy_reference_first_attempt", attempts: 1, hasReference: true, wantVisible: true},
		{name: "legacy_reference_after_retry", attempts: 2, hasReference: true},
		{name: "previous_attempt_reference", attempts: 2, referenceAttempt: 1, hasReference: true},
		{name: "current_attempt_reference", attempts: 2, referenceAttempt: 2, hasReference: true, wantVisible: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := &model.Job{ID: "job", Attempts: tc.attempts}
			wantStorage := job.ID
			if tc.hasReference {
				wantStorage = job.ID + "/.gpuflow-versions/published"
				job.ArtifactRefs = map[string]model.ArtifactReference{"training.log": {StorageID: wantStorage, Attempt: tc.referenceAttempt}}
			}
			storageID, visible := artifactStorageID(job, "training.log")
			if visible != tc.wantVisible || (visible && storageID != wantStorage) {
				t.Fatalf("storageID = %q, visible = %v; want %q, %v", storageID, visible, wantStorage, tc.wantVisible)
			}
		})
	}
}
