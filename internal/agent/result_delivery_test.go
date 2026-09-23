package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuflow/internal/model"
)

// deliveryServer exercises real HTTP upload and status acknowledgement, while
// letting tests interrupt a specific request without rerunning the workload.
type deliveryServer struct {
	t            *testing.T
	mu           sync.Mutex
	jobs         map[string]model.Job
	updates      map[string][]model.JobUpdate
	uploads      map[string]map[string][][]byte
	beforeUpload func(http.ResponseWriter, *http.Request, string, string) bool
	afterUpload  func(http.ResponseWriter, *http.Request, string, string, []byte) bool
	beforeStatus func(http.ResponseWriter, *http.Request, string, model.JobUpdate) bool
}

func newDeliveryServer(t *testing.T, jobs ...model.Job) (*deliveryServer, *httptest.Server) {
	t.Helper()
	h := &deliveryServer{t: t, jobs: make(map[string]model.Job), updates: make(map[string][]model.JobUpdate), uploads: make(map[string]map[string][][]byte)}
	for _, job := range jobs {
		h.jobs[job.ID] = job
	}
	s := httptest.NewServer(http.HandlerFunc(h.serveHTTP))
	t.Cleanup(s.Close)
	return h, s
}

func (h *deliveryServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "v1" || parts[1] != "jobs" {
		http.NotFound(w, r)
		return
	}
	id := parts[2]
	if len(parts) == 3 && r.Method == http.MethodGet {
		h.mu.Lock()
		job, ok := h.jobs[id]
		h.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(job)
		return
	}
	if len(parts) != 4 {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get(model.HeaderAgentSession) != "delivery-session" || r.Header.Get(model.HeaderAttemptToken) != "delivery-attempt" {
		h.t.Errorf("missing attempt fencing on %s", r.URL.Path)
		http.Error(w, "invalid agent session", http.StatusConflict)
		return
	}
	switch parts[3] {
	case "attempt":
		w.WriteHeader(http.StatusNoContent)
	case "artifacts":
		_, disposition, _ := mime.ParseMediaType(r.Header.Get("Content-Disposition"))
		name := disposition["filename"]
		if h.beforeUpload != nil && h.beforeUpload(w, r, id, name) {
			return
		}
		reader := io.Reader(r.Body)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			file, header, err := r.FormFile("file")
			if err != nil {
				h.t.Errorf("read multipart upload: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer file.Close()
			if r.MultipartForm != nil {
				defer r.MultipartForm.RemoveAll()
			}
			name, reader = header.Filename, file
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			h.t.Errorf("read upload %s: %v", name, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		if h.uploads[id] == nil {
			h.uploads[id] = make(map[string][][]byte)
		}
		h.uploads[id][name] = append(h.uploads[id][name], body)
		h.mu.Unlock()
		if h.afterUpload != nil && h.afterUpload(w, r, id, name, body) {
			return
		}
		w.WriteHeader(http.StatusCreated)
	case "status":
		var update model.JobUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			h.t.Errorf("decode status: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.updates[id] = append(h.updates[id], update)
		h.mu.Unlock()
		if h.beforeStatus != nil && h.beforeStatus(w, r, id, update) {
			return
		}
		h.mu.Lock()
		job := h.jobs[id]
		job.Status, job.Error, job.Output = update.Status, update.Error, update.Output
		h.jobs[id] = job
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func deliveryTestJob(id string) model.Job {
	return model.Job{ID: id, Attempts: 1, Status: model.JobRunning, AssignedNode: "delivery-node"}
}

func deliveryTestAgent(server string, timeout time.Duration) *Agent {
	a := New(Config{Server: server, ID: "delivery-node", Executor: "mock", ArtifactUploadTimeout: timeout})
	a.session = "delivery-session"
	a.setSessionLeaseDeadline(time.Now().Add(time.Minute))
	return a
}

func deliveryTestWorkspace(t *testing.T, parent string, job *model.Job, payload []byte) (*resultWorkspace, []byte) {
	t.Helper()
	w, err := newResultWorkspace(parent, "delivery-node", job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.artifacts, "checkpoint.bin"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	// This intentionally exceeds the control plane's bounded output tail.
	log := bytes.Repeat([]byte(job.ID+" complete training log\n"), 5000)
	if err := os.WriteFile(w.log, log, 0600); err != nil {
		t.Fatal(err)
	}
	return w, log
}

func deliveryArchivePayload(t *testing.T, data []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	archive := tar.NewReader(gz)
	header, err := archive.Next()
	if err != nil || header.Name != "checkpoint.bin" {
		t.Fatalf("unexpected archive entry: %v, %v", header, err)
	}
	payload, err := io.ReadAll(archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Next(); err != io.EOF {
		t.Fatalf("unexpected extra archive entry or corrupt trailer: %v", err)
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		t.Fatalf("corrupt gzip trailer: %v", err)
	}
	return payload
}

func assertDeliveryRetained(t *testing.T, w *resultWorkspace, payload, fullLog []byte, state string) {
	t.Helper()
	for name, want := range map[string][]byte{filepath.Join(w.artifacts, "checkpoint.bin"): payload, w.log: fullLog} {
		got, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("retained result %s differs or is missing: %v", name, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(w.root, "recovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest resultManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.State != state || manifest.JobID != w.manifest.JobID || manifest.Attempt != 1 {
		t.Fatalf("unexpected retained manifest: %+v", manifest)
	}
	if strings.Contains(string(data), "delivery-session") || strings.Contains(string(data), "delivery-attempt") {
		t.Fatal("recovery manifest contains an agent credential")
	}
}

func closeDeliveryConnection(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	connection, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("hijack response: %v", err)
		return
	}
	_ = connection.Close()
}

func TestConcurrentResultDeliveryKeepsArchivesAndFullLogsIsolated(t *testing.T) {
	const count = 8
	jobs := make([]model.Job, count)
	for i := range jobs {
		jobs[i] = deliveryTestJob(fmt.Sprintf("concurrent-%d", i))
	}
	h, server := newDeliveryServer(t, jobs...)
	var arrived atomic.Int32
	allArchives := make(chan struct{})
	h.afterUpload = func(w http.ResponseWriter, r *http.Request, _, name string, _ []byte) bool {
		if name == "artifacts.tar.gz" {
			if arrived.Add(1) == count {
				close(allArchives)
			}
			select {
			case <-allArchives:
			case <-r.Context().Done():
			}
		}
		return false
	}
	a := deliveryTestAgent(server.URL, 5*time.Second)
	parent := t.TempDir()
	workspaces := make([]*resultWorkspace, count)
	payloads, logs := make([][]byte, count), make([][]byte, count)
	for i := range jobs {
		payloads[i] = bytes.Repeat([]byte(jobs[i].ID+"-unique-model"), i+3)
		workspaces[i], logs[i] = deliveryTestWorkspace(t, parent, &jobs[i], payloads[i])
	}
	// Another retained attempt under the common work root must survive cleanup.
	sentinel := filepath.Join(parent, "retained-checkpoint")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	errors := make(chan error, count)
	for i := range jobs {
		go func(i int) {
			errors <- a.finishResult(context.Background(), &jobs[i], "delivery-attempt", workspaces[i], "finished", nil)
		}(i)
	}
	for range jobs {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, job := range jobs {
		archive, log := h.uploads[job.ID]["artifacts.tar.gz"], h.uploads[job.ID]["training.log"]
		if len(archive) != 1 || len(log) != 1 {
			t.Fatalf("%s upload count: archives=%d logs=%d", job.ID, len(archive), len(log))
		}
		if !bytes.Equal(deliveryArchivePayload(t, archive[0]), payloads[i]) || !bytes.Equal(log[0], logs[i]) {
			t.Fatalf("%s received another attempt's output or a truncated log", job.ID)
		}
		if len(h.updates[job.ID]) != 1 || h.updates[job.ID][0].Status != model.JobSucceeded {
			t.Fatalf("unexpected terminal updates: %+v", h.updates[job.ID])
		}
		if _, err := os.Stat(workspaces[i].root); !os.IsNotExist(err) {
			t.Fatalf("acknowledged workspace was not removed: %v", err)
		}
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("cleanup removed a sibling result: %v", err)
	}
}

func TestInterruptedArtifactDeliveryReplaysFromStartAndWaitsForAck(t *testing.T) {
	job := deliveryTestJob("interrupted-upload")
	h, server := newDeliveryServer(t, job)
	payload := make([]byte, 256<<10)
	_, _ = rand.New(rand.NewSource(42)).Read(payload)
	w, _ := deliveryTestWorkspace(t, t.TempDir(), &job, payload)
	var requests atomic.Int32
	firstPrefix := make(chan []byte, 1)
	replayed := make(chan []byte, 1)
	releaseAck := make(chan struct{})
	defer close(releaseAck)
	h.beforeUpload = func(response http.ResponseWriter, r *http.Request, _, name string) bool {
		if name != "artifacts.tar.gz" || requests.Add(1) != 1 {
			return false
		}
		prefix := make([]byte, 64)
		if _, err := io.ReadFull(r.Body, prefix); err != nil {
			t.Errorf("read interrupted request prefix: %v", err)
		}
		firstPrefix <- prefix
		closeDeliveryConnection(t, response)
		return true
	}
	h.afterUpload = func(response http.ResponseWriter, r *http.Request, _, name string, body []byte) bool {
		if name != "artifacts.tar.gz" {
			return false
		}
		replayed <- body
		select {
		case <-releaseAck:
			response.WriteHeader(http.StatusCreated)
		case <-r.Context().Done():
		}
		return true
	}
	a := deliveryTestAgent(server.URL, 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- a.finishResult(context.Background(), &job, "delivery-attempt", w, "finished", nil) }()
	var uploaded []byte
	select {
	case uploaded = <-replayed:
	case <-time.After(4 * time.Second):
		t.Fatal("interrupted upload was not retried")
	}
	if !bytes.HasPrefix(uploaded, <-firstPrefix) || !bytes.Equal(deliveryArchivePayload(t, uploaded), payload) {
		t.Fatal("retry did not replay the complete original archive")
	}
	var bundle string
	if err := filepath.Walk(w.root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Name() == "artifacts.tar.gz" {
			bundle = path
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(bundle)
	if err != nil || !bytes.Equal(uploaded, expected) {
		t.Fatalf("retry bytes differ from the complete local archive: %v", err)
	}
	h.mu.Lock()
	premature := append([]model.JobUpdate(nil), h.updates[job.ID]...)
	h.mu.Unlock()
	if len(premature) != 0 {
		t.Fatalf("terminal status sent before upload acknowledgement: %+v", premature)
	}
	select {
	case err := <-done:
		t.Fatalf("delivery completed before acknowledgement: %v", err)
	default:
	}
	releaseAck <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("unexpected archive attempts: %d", requests.Load())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.updates[job.ID]) != 1 || h.updates[job.ID][0].Status != model.JobSucceeded {
		t.Fatalf("unexpected acknowledged result: %+v", h.updates[job.ID])
	}
}

func TestUnavailableArtifactDeliveryRetainsResultsAndDisablesComputationRetry(t *testing.T) {
	for _, failingFile := range []string{"training.log", "artifacts.tar.gz"} {
		t.Run(failingFile, func(t *testing.T) {
			job := deliveryTestJob("storage-unavailable")
			h, server := newDeliveryServer(t, job)
			h.afterUpload = func(w http.ResponseWriter, _ *http.Request, _, name string, _ []byte) bool {
				if name != failingFile {
					return false
				}
				http.Error(w, "temporary storage outage", http.StatusServiceUnavailable)
				return true
			}
			a := deliveryTestAgent(server.URL, 600*time.Millisecond)
			payload := []byte("completed expensive training")
			w, log := deliveryTestWorkspace(t, t.TempDir(), &job, payload)
			if err := a.finishResult(context.Background(), &job, "delivery-attempt", w, "computed", nil); err == nil {
				t.Fatal("unacknowledged result delivery returned success")
			}
			assertDeliveryRetained(t, w, payload, log, "delivery_failed")
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.uploads[job.ID][failingFile]) < 2 {
				t.Fatal("transient storage failure was not retried within the delivery window")
			}
			updates := h.updates[job.ID]
			if len(updates) != 1 || updates[0].Status != model.JobFailed || updates[0].Retryable == nil || *updates[0].Retryable {
				t.Fatalf("delivery failure may re-execute the completed computation: %+v", updates)
			}
			if !strings.Contains(updates[0].Output, w.root) || !strings.Contains(updates[0].Error, "result delivery failed") {
				t.Fatalf("operator cannot locate retained results: %+v", updates[0])
			}
		})
	}
}

func TestFencedArtifactDeliveryDoesNotRetryOrPublishSuccess(t *testing.T) {
	job := deliveryTestJob("superseded-upload")
	h, server := newDeliveryServer(t, job)
	var requests atomic.Int32
	h.afterUpload = func(w http.ResponseWriter, _ *http.Request, id, _ string, _ []byte) bool {
		requests.Add(1)
		h.mu.Lock()
		latest := h.jobs[id]
		latest.Attempts, latest.Status = 2, model.JobAssigned
		h.jobs[id] = latest
		h.mu.Unlock()
		http.Error(w, "attempt lease rejected", http.StatusConflict)
		return true
	}
	h.beforeStatus = func(w http.ResponseWriter, _ *http.Request, _ string, _ model.JobUpdate) bool {
		http.Error(w, "attempt lease rejected", http.StatusConflict)
		return true
	}
	a := deliveryTestAgent(server.URL, time.Second)
	payload := []byte("old attempt results")
	w, log := deliveryTestWorkspace(t, t.TempDir(), &job, payload)
	if err := a.finishResult(context.Background(), &job, "delivery-attempt", w, "computed", nil); err == nil {
		t.Fatal("rejected upload returned success")
	}
	assertDeliveryRetained(t, w, payload, log, "delivery_failed")
	if requests.Load() != 1 {
		t.Fatalf("permanent fencing failure retried %d times", requests.Load())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, update := range h.updates[job.ID] {
		if update.Status == model.JobSucceeded {
			t.Fatal("stale worker published success")
		}
	}
}

func TestCanceledAndInterruptedResultsRemainRecoverable(t *testing.T) {
	for _, canceled := range []bool{true, false} {
		name := "agent-interrupted"
		if canceled {
			name = "user-canceled"
		}
		t.Run(name, func(t *testing.T) {
			job := deliveryTestJob(name)
			if canceled {
				job.Status = model.JobCanceling
			}
			h, server := newDeliveryServer(t, job)
			a := deliveryTestAgent(server.URL, time.Second)
			payload := []byte("last recoverable checkpoint")
			w, log := deliveryTestWorkspace(t, t.TempDir(), &job, payload)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runErr, state := errJobCanceled, "canceled"
			if !canceled {
				cancel()
				runErr, state = ctx.Err(), "interrupted"
			}
			err := a.finishResult(ctx, &job, "delivery-attempt", w, "partial", runErr)
			if canceled && err != nil {
				t.Fatal(err)
			}
			if !canceled && err == nil {
				t.Fatal("interrupted delivery returned success")
			}
			assertDeliveryRetained(t, w, payload, log, state)
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.uploads[job.ID]) != 0 {
				t.Fatal("canceled/interrupted result started an upload")
			}
			if canceled && (len(h.updates[job.ID]) != 1 || h.updates[job.ID][0].Status != model.JobCanceled) {
				t.Fatalf("cancellation was not acknowledged: %+v", h.updates[job.ID])
			}
			if !canceled && len(h.updates[job.ID]) != 0 {
				t.Fatal("interrupted agent sent a terminal update")
			}
		})
	}
}

func TestCancellationDuringUploadRetainsCheckpointAndAcknowledgesCancellation(t *testing.T) {
	job := deliveryTestJob("canceled-during-upload")
	h, server := newDeliveryServer(t, job)
	h.afterUpload = func(_ http.ResponseWriter, r *http.Request, id, name string, _ []byte) bool {
		if name != "artifacts.tar.gz" {
			return false
		}
		h.mu.Lock()
		latest := h.jobs[id]
		latest.Status = model.JobCanceling
		h.jobs[id] = latest
		h.mu.Unlock()
		// The archive request has reached storage, but has not been published.
		// Polling cancellation must stop this request and preserve local output.
		<-r.Context().Done()
		return true
	}
	a := deliveryTestAgent(server.URL, 3*time.Second)
	payload := []byte("checkpoint exists before the cancellation")
	w, log := deliveryTestWorkspace(t, t.TempDir(), &job, payload)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := a.finishResult(ctx, &job, "delivery-attempt", w, "computed", nil); err != nil {
		t.Fatalf("cancellation was not acknowledged: %v", err)
	}
	assertDeliveryRetained(t, w, payload, log, "delivery_failed")
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.updates[job.ID]) != 1 || h.updates[job.ID][0].Status != model.JobCanceled {
		t.Fatalf("upload cancellation published the wrong terminal state: %+v", h.updates[job.ID])
	}
}

func TestResultCleanupRequiresConfirmationOfSameAttempt(t *testing.T) {
	for _, newerAttempt := range []bool{false, true} {
		name := "same-attempt-lost-ack"
		if newerAttempt {
			name = "newer-attempt-completed"
		}
		t.Run(name, func(t *testing.T) {
			job := deliveryTestJob(name)
			h, server := newDeliveryServer(t, job)
			h.beforeStatus = func(w http.ResponseWriter, _ *http.Request, id string, update model.JobUpdate) bool {
				h.mu.Lock()
				latest := h.jobs[id]
				latest.Status = update.Status
				if newerAttempt {
					latest.Attempts++
				}
				h.jobs[id] = latest
				h.mu.Unlock()
				// The terminal result may have committed, but the Agent never
				// received its HTTP acknowledgement and must reconcile.
				closeDeliveryConnection(t, w)
				return true
			}
			a := deliveryTestAgent(server.URL, time.Second)
			payload := []byte("completed model")
			w, log := deliveryTestWorkspace(t, t.TempDir(), &job, payload)
			if err := a.finishResult(context.Background(), &job, "delivery-attempt", w, "computed", nil); err != nil {
				t.Fatal(err)
			}
			if newerAttempt {
				assertDeliveryRetained(t, w, payload, log, "terminal_superseded_or_unconfirmed")
			} else if _, err := os.Stat(w.root); !os.IsNotExist(err) {
				t.Fatalf("confirmed same-attempt success did not clean workspace: %v", err)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.updates[job.ID]) != 1 {
				t.Fatalf("terminal request unexpectedly replayed: %+v", h.updates[job.ID])
			}
		})
	}
}
