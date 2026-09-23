package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"gpuflow/internal/model"
)

var errResultCapture = errors.New("result capture failed")

// A workspace is never shared by two attempts. It is retained unless uploads
// AND the terminal state were acknowledged. No credentials are written here.
type resultWorkspace struct {
	root, artifacts, log string
	manifest             resultManifest
}

type resultManifest struct {
	Schema    int       `json:"schema"`
	JobID     string    `json:"job_id"`
	Attempt   int       `json:"attempt"`
	NodeID    string    `json:"node_id"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
	Error     string    `json:"error,omitempty"`
}

func newResultWorkspace(parent, nodeID string, job *model.Job) (*resultWorkspace, error) {
	if parent != "" {
		if err := os.MkdirAll(parent, 0700); err != nil {
			return nil, fmt.Errorf("create result work directory: %w", err)
		}
	}
	root, err := os.MkdirTemp(parent, "gpuflow-result-"+job.ID+"-")
	if err != nil {
		return nil, fmt.Errorf("create result workspace: %w", err)
	}
	w := &resultWorkspace{
		root: root, artifacts: filepath.Join(root, "artifacts"), log: filepath.Join(root, "training.log"),
		manifest: resultManifest{Schema: 1, JobID: job.ID, Attempt: job.Attempts, NodeID: nodeID},
	}
	if err := os.Mkdir(w.artifacts, 0755); err != nil {
		return nil, fmt.Errorf("create artifacts directory in %s: %w", root, err)
	}
	if err := w.record("executing", nil); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *resultWorkspace) record(state string, cause error) error {
	w.manifest.State, w.manifest.UpdatedAt, w.manifest.Error = state, time.Now().UTC(), ""
	if cause != nil {
		w.manifest.Error = cause.Error()
	}
	data, err := json.MarshalIndent(w.manifest, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(w.root, ".recovery-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(append(data, '\n')); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, filepath.Join(w.root, "recovery.json"))
	}
	return err
}

func (w *resultWorkspace) retain(state string, cause error) string {
	if err := w.record(state, cause); err != nil {
		fmt.Printf("agent result manifest warning: %v\n", err)
	}
	message := fmt.Sprintf("results retained on node %s at %s", w.manifest.NodeID, w.root)
	fmt.Printf("agent: %s\n", message)
	return message
}

func (a *Agent) finishResult(ctx context.Context, job *model.Job, attemptToken string, w *resultWorkspace, output string, runErr error) error {
	if errors.Is(runErr, errJobCanceled) {
		// Cancellation releases the allocation promptly, but preserves any
		// checkpoint and complete log for explicit recovery by the operator.
		output = appendOutput(output, w.retain("canceled", runErr))
		return a.ackCanceled(ctx, job.ID, job.Attempts, attemptToken, output)
	}
	if ctx.Err() != nil {
		w.retain("interrupted", ctx.Err())
		return ctx.Err()
	}
	if errors.Is(runErr, errResultCapture) {
		return a.failResultDelivery(ctx, job, attemptToken, w, output, runErr)
	}
	if err := w.record("uploading", runErr); err != nil {
		return a.failResultDelivery(ctx, job, attemptToken, w, output, errors.Join(runErr, err))
	}
	// Both the complete log and any nonempty artifacts are required results.
	// A transient S3/network error retries delivery, never the computation.
	if err := a.uploadCompleteLog(ctx, job.ID, attemptToken, w.log); err != nil {
		return a.failResultDelivery(ctx, job, attemptToken, w, output, errors.Join(runErr, fmt.Errorf("complete log delivery: %w", err)))
	}
	bundle, err := archiveArtifacts(w.artifacts)
	if err == nil && bundle != "" {
		err = a.uploadArtifact(ctx, job.ID, attemptToken, bundle)
	}
	if err != nil {
		return a.failResultDelivery(ctx, job, attemptToken, w, output, errors.Join(runErr, fmt.Errorf("artifact delivery: %w", err)))
	}
	if bundle != "" {
		output = appendOutput(output, "artifact uploaded: artifacts.tar.gz")
	}
	update := model.JobUpdate{Status: model.JobSucceeded, Output: output}
	if runErr != nil {
		update.Status, update.Error = model.JobFailed, runErr.Error()
	}
	if err := a.updateJobStatusUntilAccepted(ctx, job, attemptToken, update); err != nil {
		w.retain("terminal_unconfirmed", err)
		return err
	}
	// updateJobStatusUntilAccepted also returns when a newer transition won.
	// Retain old results on cancellation, takeover or an uncertain final read.
	var latest model.Job
	if err := a.getJob(ctx, job.ID, attemptToken, &latest); err != nil || latest.Attempts != job.Attempts || latest.Status != update.Status {
		w.retain("terminal_superseded_or_unconfirmed", err)
		return nil
	}
	if err := os.RemoveAll(w.root); err != nil {
		return fmt.Errorf("results delivered but workspace cleanup failed: %w", err)
	}
	return nil
}

func (a *Agent) failResultDelivery(ctx context.Context, job *model.Job, attemptToken string, w *resultWorkspace, output string, cause error) error {
	output = appendOutput(output, w.retain("delivery_failed", cause))
	if ctx.Err() != nil {
		return errors.Join(cause, ctx.Err())
	}
	var latest model.Job
	if err := a.getJob(ctx, job.ID, attemptToken, &latest); err == nil {
		if latest.Attempts != job.Attempts {
			return cause
		}
		if latest.Status == model.JobCanceling || latest.Status == model.JobCanceled {
			return a.ackCanceled(ctx, job.ID, job.Attempts, attemptToken, output)
		}
		if latest.Status != model.JobRunning {
			return cause
		}
	}
	noRetry := false
	update := model.JobUpdate{Status: model.JobFailed, Output: output, Error: "result delivery failed; local results retained: " + cause.Error(), Retryable: &noRetry}
	return errors.Join(cause, a.updateJobStatusUntilAccepted(ctx, job, attemptToken, update))
}

func retryableArtifactStatus(status int) bool {
	return status == 0 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func (a *Agent) retryArtifactUpload(ctx context.Context, jobID, attemptToken, file string) error {
	delay := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		status, err := a.uploadArtifactFile(ctx, "/v1/jobs/"+url.PathEscape(jobID)+"/artifacts?node_id="+url.QueryEscape(a.cfg.ID), file, a.jobHeaders(attemptToken))
		if err == nil {
			return nil
		}
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return err
		}
		if !retryableArtifactStatus(status) || ctx.Err() != nil {
			return err
		}
		// The server checks session/attempt fencing for every fresh upload and
		// again before publication. Retry bytes, never bypass those checks.
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
		if delay < 8*time.Second {
			delay *= 2
		}
	}
}
