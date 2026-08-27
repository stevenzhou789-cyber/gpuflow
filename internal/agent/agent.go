package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuflow/internal/client"
	"gpuflow/internal/model"
	"gpuflow/pkg/edition"
)

type Config struct {
	Server, Token, ID, Name, Provider, Pool, Executor, ArtifactDir, ProbeImage string
	CPUCores                                                                   int
	HourlyPrice                                                                float64
	PollInterval, HeartbeatInterval, HealthInterval, ArtifactUploadTimeout     time.Duration
	ProbeCommand                                                               func(context.Context, string, ...string) ([]byte, error)
}
type Agent struct {
	cfg           Config
	client        *client.Client
	session       string
	baseline      model.Node
	workerTargets chan int
	failStop      context.CancelCauseFunc
	sessionTTL    time.Duration
	leaseStarted  time.Time
}

var (
	errJobCanceled              = errors.New("job canceled")
	errAgentSessionLeaseExpired = errors.New("agent session lease expired")
)

func New(cfg Config) *Agent {
	if cfg.CPUCores <= 0 {
		cfg.CPUCores = runtime.NumCPU()
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 3 * time.Second
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 10 * time.Second
	}
	if cfg.HealthInterval == 0 {
		cfg.HealthInterval = time.Minute
	}
	if cfg.ArtifactUploadTimeout == 0 {
		cfg.ArtifactUploadTimeout = 30 * time.Minute
	}
	return &Agent{cfg: cfg, client: client.New(cfg.Server, cfg.Token), sessionTTL: model.AgentSessionFailStopTTL}
}

func (a *Agent) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	a.failStop = cancel
	a.workerTargets = make(chan int, 1)

	sessionBytes := make([]byte, 16)
	if _, err := rand.Read(sessionBytes); err != nil {
		return fmt.Errorf("generate agent session: %w", err)
	}
	a.session = hex.EncodeToString(sessionBytes)

	var descriptor edition.Descriptor
	if _, err := a.doContext(runCtx, http.MethodGet, "/v1/capabilities", nil, &descriptor, a.sessionHeaders()); err != nil {
		return fmt.Errorf("load server capabilities: %w", err)
	}
	if descriptor.SchemaVersion != edition.CapabilitiesSchemaVersion || descriptor.Features == nil {
		return fmt.Errorf("invalid server capabilities schema: expected version %d", edition.CapabilitiesSchemaVersion)
	}
	if _, ok := descriptor.Features[edition.FeatureBasicScheduler]; !ok {
		return errors.New("invalid server capabilities: basic_scheduler feature is missing")
	}
	if descriptor.Features[edition.FeatureNodeHealth] && strings.TrimSpace(a.cfg.ProbeImage) == "" {
		a.cfg.ProbeImage = strings.TrimSpace(descriptor.AgentImage)
		if a.cfg.ProbeImage == "" {
			return errors.New("server enables node health but does not provide an agent image for GPU probing")
		}
	}

	n, err := a.probeNode(runCtx)
	if err != nil {
		return fmt.Errorf("discover node resources: %w", err)
	}
	if !descriptor.Features[edition.FeaturePerGPUInventory] {
		n.Devices, n.DriverVersion, n.DockerVersion = nil, "", ""
	}
	if !descriptor.Features[edition.FeatureNodeHealth] {
		n.HealthStatus, n.HealthReason, n.LastHealthCheck = "", "", nil
	}
	a.leaseStarted = time.Now()
	if _, err := a.doContext(runCtx, http.MethodPost, "/v1/nodes/register", n, &n, a.sessionHeaders()); err != nil {
		return fmt.Errorf("register node: %w", err)
	}
	a.cfg.ID = n.ID
	a.baseline = n
	// Confirm a fresh server-side lease before any worker can start. This also
	// closes the window where a slow registration response consumes most of the
	// lease before the Agent begins its heartbeat loop.
	requestStarted := time.Now()
	requestTimeout := a.cfg.HeartbeatInterval
	if requestTimeout <= 0 || requestTimeout > a.sessionTTL {
		requestTimeout = a.sessionTTL
	}
	heartbeatCtx, heartbeatCancel := context.WithTimeout(runCtx, requestTimeout)
	_, heartbeatErr := a.doContext(heartbeatCtx, http.MethodPost, "/v1/nodes/"+a.cfg.ID+"/heartbeat", nil, nil, a.sessionHeaders())
	heartbeatCancel()
	if heartbeatErr != nil {
		if cause := context.Cause(runCtx); cause != nil {
			return cause
		}
		return fmt.Errorf("confirm agent session heartbeat: %w", heartbeatErr)
	}
	a.leaseStarted = requestStarted
	workers := n.GPUCount
	if workers < 1 {
		workers = 1
	}
	var group sync.WaitGroup
	group.Add(1)
	go func() { defer group.Done(); a.workerSupervisor(runCtx, workers) }()
	group.Add(1)
	go func() { defer group.Done(); a.heartbeatLoop(runCtx) }()
	if descriptor.Features[edition.FeatureNodeHealth] {
		group.Add(1)
		go func() { defer group.Done(); a.healthLoop(runCtx) }()
	}
	<-runCtx.Done()
	group.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return context.Cause(runCtx)
}

func (a *Agent) workerSupervisor(ctx context.Context, initial int) {
	var workers sync.WaitGroup
	count := 0
	startTo := func(target int) {
		if target < 1 {
			target = 1
		}
		for count < target {
			count++
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() {
					if recovered := recover(); recovered != nil && a.failStop != nil {
						a.failStop(fmt.Errorf("agent worker panic: %v", recovered))
					}
				}()
				a.runWorker(ctx)
			}()
		}
	}
	startTo(initial)
	for {
		select {
		case <-ctx.Done():
			workers.Wait()
			return
		case target := <-a.workerTargets:
			startTo(target)
		}
	}
}

func (a *Agent) healthLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.HealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			node, err := a.probeNode(ctx)
			if err == nil && a.baseline.GPUCount > 0 && node.GPUCount < a.baseline.GPUCount {
				err = fmt.Errorf("GPU inventory dropped from %d to %d device(s)", a.baseline.GPUCount, node.GPUCount)
			}
			updateNode := node
			if err != nil {
				// A transient probe failure or missing GPU must not replace known GPU
				// capacity with a CPU-only inventory. The server also preserves the
				// last healthy inventory for DEGRADED updates.
				updateNode = a.baseline
			}
			update := model.NodeHealthUpdate{Status: "HEALTHY", Devices: updateNode.Devices, GPUModel: updateNode.GPUModel, GPUCount: updateNode.GPUCount, VRAMGB: updateNode.VRAMGB, DriverVersion: updateNode.DriverVersion, DockerVersion: updateNode.DockerVersion}
			if err != nil {
				update.Status, update.Reason = "DEGRADED", err.Error()
			}
			if _, updateErr := a.doContext(ctx, http.MethodPost, "/v1/nodes/"+a.cfg.ID+"/health", update, nil, a.sessionHeaders()); updateErr != nil && ctx.Err() == nil {
				fmt.Printf("agent health update warning: %v\n", updateErr)
			} else if updateErr == nil && err == nil {
				a.baseline = node
				select {
				case a.workerTargets <- node.GPUCount:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func (a *Agent) sessionHeaders() http.Header {
	headers := make(http.Header, 1)
	headers.Set(model.HeaderAgentSession, a.session)
	return headers
}

func (a *Agent) jobHeaders(attemptToken string) http.Header {
	headers := a.sessionHeaders()
	headers.Set(model.HeaderAttemptToken, attemptToken)
	return headers
}

func (a *Agent) doContext(ctx context.Context, method, path string, body, out any, headers http.Header) (int, error) {
	status, err := a.client.DoContextWithHeaders(ctx, method, path, body, out, headers)
	a.failStopIfSessionRejected(status, err)
	return status, err
}

func (a *Agent) uploadArtifactFile(ctx context.Context, path, filePath string, headers http.Header) (int, error) {
	status, err := a.client.UploadArtifactContextWithHeaders(ctx, path, filePath, headers)
	a.failStopIfSessionRejected(status, err)
	return status, err
}

func (a *Agent) failStopIfSessionRejected(status int, err error) {
	if status != http.StatusConflict || err == nil || a.failStop == nil {
		return
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "invalid agent session") || strings.Contains(message, "another agent session") {
		a.failStop(fmt.Errorf("agent session rejected by control plane: %w", err))
	}
}

func (a *Agent) runWorker(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := a.tick(ctx); err != nil {
			fmt.Printf("agent warning: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *Agent) tick(ctx context.Context) error {
	var dispatched model.AgentJob
	status, err := a.doContext(ctx, http.MethodPost, "/v1/nodes/"+a.cfg.ID+"/next", nil, &dispatched, a.sessionHeaders())
	if err != nil {
		return err
	}
	if status == http.StatusNoContent {
		return nil
	}
	attemptToken := strings.TrimSpace(dispatched.AttemptToken)
	if attemptToken == "" {
		return errors.New("server dispatched a job without an attempt token")
	}
	job := dispatched.Job
	if job.Status == model.JobCanceling {
		return a.ackCanceled(ctx, job.ID, job.Attempts, attemptToken, "")
	}
	if job.Status != model.JobAssigned {
		return fmt.Errorf("server dispatched job %s in unsupported state %s", job.ID, job.Status)
	}
	if err = a.updateJobStatusUntilAccepted(ctx, &job, attemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
		// The container must never start unless the control plane accepted this
		// exact session and attempt as RUNNING.
		return err
	}
	if a.cfg.ArtifactDir != "" {
		if err := os.MkdirAll(a.cfg.ArtifactDir, 0o755); err != nil {
			return a.failStartedJob(ctx, &job, attemptToken, fmt.Errorf("create artifact work directory: %w", err))
		}
	}
	artifactDir, err := os.MkdirTemp(a.cfg.ArtifactDir, "gpuflow-artifacts-"+job.ID+"-")
	if err != nil {
		return a.failStartedJob(ctx, &job, attemptToken, fmt.Errorf("create artifact directory: %w", err))
	}
	defer os.RemoveAll(artifactDir)
	logDir, err := os.MkdirTemp(a.cfg.ArtifactDir, "gpuflow-log-"+job.ID+"-")
	if err != nil {
		return a.failStartedJob(ctx, &job, attemptToken, fmt.Errorf("create log directory: %w", err))
	}
	defer os.RemoveAll(logDir)
	logPath := filepath.Join(logDir, "training.log")
	output, runErr := a.execute(ctx, &job, attemptToken, artifactDir, logPath)
	if cleanupErr := a.cleanupJobContainersUntilDone(ctx, job.ID, job.Attempts); cleanupErr != nil {
		return cleanupErr
	}
	if errors.Is(runErr, errJobCanceled) {
		// The container is already stopped and waited. Release its GPU lease
		// immediately; a slow object store must not hold cancellation open.
		return a.ackCanceled(ctx, job.ID, job.Attempts, attemptToken, output)
	}
	if uploadErr := a.uploadCompleteLog(ctx, job.ID, attemptToken, logPath); uploadErr != nil {
		output = appendOutput(output, "complete log upload warning: "+uploadErr.Error())
	}
	var latest model.Job
	if statusErr := a.getJob(ctx, job.ID, attemptToken, &latest); statusErr == nil && latest.Status == model.JobCanceling {
		return a.ackCanceled(ctx, job.ID, job.Attempts, attemptToken, output)
	}
	if bundle, bundleErr := archiveArtifacts(artifactDir); bundleErr != nil {
		output = appendOutput(output, "artifact packaging warning: "+bundleErr.Error())
	} else if bundle != "" {
		defer os.Remove(bundle)
		if uploadErr := a.uploadArtifact(ctx, job.ID, attemptToken, bundle); uploadErr != nil {
			output = appendOutput(output, "artifact upload warning: "+uploadErr.Error())
		} else {
			output = appendOutput(output, "artifact uploaded: artifacts.tar.gz")
		}
	}
	if statusErr := a.getJob(ctx, job.ID, attemptToken, &latest); statusErr == nil && latest.Status == model.JobCanceling {
		return a.ackCanceled(ctx, job.ID, job.Attempts, attemptToken, output)
	}
	update := model.JobUpdate{Status: model.JobSucceeded, Output: output}
	if runErr != nil {
		update.Status = model.JobFailed
		update.Error = runErr.Error()
	}
	return a.updateJobStatusUntilAccepted(ctx, &job, attemptToken, update)
}

func (a *Agent) failStartedJob(ctx context.Context, job *model.Job, attemptToken string, cause error) error {
	if err := a.cleanupJobContainersUntilDone(ctx, job.ID, job.Attempts); err != nil {
		return fmt.Errorf("%v; cleanup before failed status: %w", cause, err)
	}
	update := model.JobUpdate{Status: model.JobFailed, Error: cause.Error()}
	if err := a.updateJobStatusUntilAccepted(ctx, job, attemptToken, update); err != nil {
		return fmt.Errorf("%v; persist failed status: %w", cause, err)
	}
	return cause
}

func (a *Agent) updateJobStatusUntilAccepted(ctx context.Context, job *model.Job, attemptToken string, update model.JobUpdate) error {
	path := "/v1/jobs/" + job.ID + "/status?node_id=" + a.cfg.ID
	deadline := time.Now().Add(model.AgentSessionTTL)
	for {
		status, err := a.doContext(ctx, http.MethodPost, path, update, nil, a.jobHeaders(attemptToken))
		if err == nil {
			return nil
		}
		var latest model.Job
		if getErr := a.getJob(ctx, job.ID, attemptToken, &latest); getErr == nil {
			if jobStatusAccepted(update.Status, latest.Status) {
				return nil
			}
			if update.Status == model.JobRunning && latest.Status != model.JobAssigned {
				return fmt.Errorf("job %s is no longer startable (status %s): %w", job.ID, latest.Status, err)
			}
			if update.Status != model.JobRunning && latest.Status == model.JobCanceling {
				// Cancellation won the race with this terminal write. The current
				// attempt still owns cleanup and must release the lease with a
				// CANCELED acknowledgement instead of retrying the old terminal state.
				if cleanupErr := a.cleanupJobContainer(job.ID, job.Attempts); cleanupErr != nil {
					err = fmt.Errorf("clean concurrently canceled job container: %w", cleanupErr)
				} else if update.Status != model.JobCanceled {
					update.Status, update.Error = model.JobCanceled, ""
					continue
				}
			}
			if update.Status != model.JobRunning && latest.Status != model.JobRunning && latest.Status != model.JobCanceling {
				// Another valid transition (retry, takeover, or completion) has
				// superseded this terminal write. The stale worker must stop.
				return nil
			}
		}
		if status == http.StatusPreconditionFailed || ctx.Err() != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			commitErr := fmt.Errorf("job %s status %s was not accepted within %s: %w", job.ID, update.Status, model.AgentSessionTTL, err)
			if a.failStop != nil {
				a.failStop(commitErr)
			}
			return commitErr
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func jobStatusAccepted(requested, actual model.JobStatus) bool {
	switch requested {
	case model.JobRunning:
		return actual == model.JobRunning
	case model.JobSucceeded:
		return actual == model.JobSucceeded
	case model.JobCanceled:
		return actual == model.JobCanceled
	case model.JobFailed:
		return actual == model.JobFailed || actual == model.JobQueued || actual == model.JobAssigned
	default:
		return false
	}
}

func (a *Agent) getJob(ctx context.Context, jobID, attemptToken string, out *model.Job) error {
	_, err := a.doContext(ctx, http.MethodGet, "/v1/jobs/"+jobID, nil, out, a.jobHeaders(attemptToken))
	return err
}

func (a *Agent) ackCanceled(ctx context.Context, jobID string, attempts int, attemptToken, output string) error {
	if err := a.cleanupJobContainersUntilDone(ctx, jobID, attempts); err != nil {
		return err
	}
	job := model.Job{ID: jobID, Attempts: attempts}
	return a.updateJobStatusUntilAccepted(ctx, &job, attemptToken, model.JobUpdate{Status: model.JobCanceled, Output: output})
}

func (a *Agent) cleanupJobContainersUntilDone(ctx context.Context, jobID string, attempts int) error {
	deadline := time.Now().Add(model.AgentSessionTTL)
	for {
		if err := a.cleanupJobAttemptContainers(jobID, attempts); err == nil {
			break
		} else if !time.Now().Before(deadline) {
			cleanupErr := fmt.Errorf("clean job containers within %s: %w", model.AgentSessionTTL, err)
			if a.failStop != nil {
				a.failStop(cleanupErr)
			}
			return cleanupErr
		} else {
			fmt.Printf("agent canceled container cleanup warning: %v\n", err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func (a *Agent) uploadArtifact(parent context.Context, jobID, attemptToken, bundle string) error {
	ctx, cancel := context.WithTimeout(parent, a.cfg.ArtifactUploadTimeout)
	defer cancel()
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var latest model.Job
				if err := a.getJob(ctx, jobID, attemptToken, &latest); err == nil && (latest.Status == model.JobCanceling || latest.Status == model.JobCanceled) {
					cancel()
					return
				}
			}
		}
	}()
	_, err := a.uploadArtifactFile(ctx, "/v1/jobs/"+jobID+"/artifacts?node_id="+a.cfg.ID, bundle, a.jobHeaders(attemptToken))
	cancel()
	<-monitorDone
	return err
}

func (a *Agent) uploadCompleteLog(parent context.Context, jobID, attemptToken, logPath string) error {
	return a.uploadArtifact(parent, jobID, attemptToken, logPath)
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.HeartbeatInterval)
	defer ticker.Stop()
	ttl := a.sessionTTL
	if ttl <= 0 {
		ttl = model.AgentSessionFailStopTTL
	}
	leaseDeadline := a.leaseStarted.Add(ttl)
	if a.leaseStarted.IsZero() {
		leaseDeadline = time.Now().Add(ttl)
	}
	initialLease := time.Until(leaseDeadline)
	if initialLease <= 0 {
		a.stopExpiredSession(ttl)
		return
	}
	leaseTimer := time.NewTimer(initialLease)
	defer leaseTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-leaseTimer.C:
			a.stopExpiredSession(ttl)
			return
		case <-ticker.C:
			remaining := time.Until(leaseDeadline)
			if remaining <= 0 {
				a.stopExpiredSession(ttl)
				return
			}
			requestTimeout := a.cfg.HeartbeatInterval
			if requestTimeout <= 0 || requestTimeout > remaining {
				requestTimeout = remaining
			}
			requestStarted := time.Now()
			requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
			_, err := a.doContext(requestCtx, http.MethodPost, "/v1/nodes/"+a.cfg.ID+"/heartbeat", nil, nil, a.sessionHeaders())
			cancel()
			if err == nil {
				leaseDeadline = requestStarted.Add(ttl)
				remaining = time.Until(leaseDeadline)
				if remaining <= 0 {
					a.stopExpiredSession(ttl)
					return
				}
				if !leaseTimer.Stop() {
					select {
					case <-leaseTimer.C:
					default:
					}
				}
				leaseTimer.Reset(remaining)
			} else if ctx.Err() == nil {
				fmt.Printf("agent heartbeat warning: %v\n", err)
				if !time.Now().Before(leaseDeadline) {
					a.stopExpiredSession(ttl)
					return
				}
			}
		}
	}
}

func (a *Agent) stopExpiredSession(ttl time.Duration) {
	if a.failStop != nil {
		a.failStop(fmt.Errorf("%w after %s without a successful heartbeat", errAgentSessionLeaseExpired, ttl))
	}
}

func (a *Agent) execute(parent context.Context, job *model.Job, attemptToken, artifactDir, logPath string) (string, error) {
	if a.cfg.Executor == "mock" {
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-parent.Done():
			return "", parent.Err()
		case <-timer.C:
		}
		if err := os.WriteFile(logPath, []byte("mock executor completed\n"), 0o600); err != nil {
			return "", fmt.Errorf("create complete job log: %w", err)
		}
		return "mock executor completed", nil
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(job.TimeoutSeconds)*time.Second)
	defer cancel()
	args := []string{"run", "--rm", "--label", "gpuflow.job=" + job.ID}
	args = append(args, "--mount", "type=bind,source="+artifactDir+",target=/gpuflow/artifacts", "-e", "GPUFLOW_ARTIFACT_DIR=/gpuflow/artifacts")
	args = append(args, "-e", "PYTHONUNBUFFERED=1")
	if job.Requirements.GPUCount > 0 {
		args = append(args, "--gpus", dockerGPUSelector(job))
	}
	for k, v := range job.Environment {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, job.Image)
	args = append(args, job.Command...)
	containerName := jobContainerName(job.ID, job.Attempts)
	// A previous agent process may have exited while Docker kept the job
	// container alive. Recovered attempts use a new name, but still remove the
	// legacy name and the immediately preceding attempt before starting.
	if err := a.cleanupBeforeAttempt(job); err != nil {
		return "", fmt.Errorf("clean recovered job containers: %w", err)
	}
	args = append([]string{"run", "--name", containerName}, args[1:]...)
	cmd := exec.Command("docker", args...)
	logFile, logErr := os.Create(logPath)
	if logErr != nil {
		return "", fmt.Errorf("create complete job log: %w", logErr)
	}
	defer logFile.Close()
	output := &liveJobLog{full: logFile}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("docker run failed: %w", err)
	}
	// Docker keeps containers alive if its client disappears. This cleanup is
	// therefore registered immediately after Start so panic/unwind cannot leave
	// an unfenced workload consuming the GPU.
	defer func() {
		if cleanupErr := cleanupContainer(containerName); cleanupErr != nil {
			fmt.Printf("agent container cleanup warning: %v\n", cleanupErr)
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var err error
	var lastUploaded string
	var lastUpload time.Time
	for {
		select {
		case err = <-done:
			goto finished
		case <-ctx.Done():
			immediate := errors.Is(context.Cause(parent), errAgentSessionLeaseExpired)
			if terminateErr := terminateContainer(containerName, !immediate); terminateErr != nil {
				err = terminateErr
			} else {
				err = waitDockerCommand(cmd, done)
			}
			goto finished
		case <-ticker.C:
			current := output.String()
			if current != lastUploaded && time.Since(lastUpload) >= 2*time.Second {
				if uploadErr := a.uploadLiveLog(parent, job.ID, attemptToken, current); uploadErr != nil {
					fmt.Printf("agent log upload warning: %v\n", uploadErr)
				} else {
					lastUploaded, lastUpload = current, time.Now()
				}
			}
			var latest model.Job
			if pollErr := a.getJob(parent, job.ID, attemptToken, &latest); pollErr == nil && (latest.Status == model.JobCanceling || latest.Status == model.JobCanceled) {
				if terminateErr := terminateContainer(containerName, true); terminateErr != nil {
					text := strings.TrimSpace(output.String())
					return text, fmt.Errorf("terminate canceled job container: %w", terminateErr)
				}
				if waitErr := waitDockerCommand(cmd, done); waitErr != nil {
					fmt.Printf("agent docker client cleanup warning: %v\n", waitErr)
				}
				text := strings.TrimSpace(output.String())
				return text, errJobCanceled
			}
		}
	}

finished:
	text := output.String()
	if len(text) > 65536 {
		text = text[len(text)-65536:]
	}
	if ctx.Err() == context.DeadlineExceeded {
		return text, fmt.Errorf("job exceeded timeout of %d seconds", job.TimeoutSeconds)
	}
	if err != nil {
		return text, fmt.Errorf("docker run failed: %w", err)
	}
	return strings.TrimSpace(text), nil
}

func dockerGPUSelector(job *model.Job) string {
	if len(job.AllocatedGPUs) == 0 {
		return strconv.Itoa(job.Requirements.GPUCount)
	}
	indices := make([]string, len(job.AllocatedGPUs))
	for index, gpu := range job.AllocatedGPUs {
		indices[index] = strconv.Itoa(gpu)
	}
	return `"device=` + strings.Join(indices, ",") + `"`
}

func jobContainerName(jobID string, attempts int) string {
	return "gpuflow-job-" + jobID + "-" + strconv.Itoa(attempts)
}

func legacyJobContainerName(jobID string) string {
	return "gpuflow-job-" + jobID
}

func (a *Agent) cleanupBeforeAttempt(job *model.Job) error {
	if a.cfg.Executor == "mock" {
		return nil
	}
	if err := cleanupContainer(legacyJobContainerName(job.ID)); err != nil {
		return err
	}
	if job.Attempts > 1 {
		if err := cleanupContainer(jobContainerName(job.ID, job.Attempts-1)); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) cleanupJobContainer(jobID string, attempts int) error {
	if a.cfg.Executor == "mock" {
		return nil
	}
	if err := cleanupContainer(jobContainerName(jobID, attempts)); err != nil {
		return err
	}
	return cleanupContainer(legacyJobContainerName(jobID))
}

func (a *Agent) cleanupJobAttemptContainers(jobID string, attempts int) error {
	if err := a.cleanupJobContainer(jobID, attempts); err != nil {
		return err
	}
	if a.cfg.Executor != "mock" && attempts > 1 {
		return cleanupContainer(jobContainerName(jobID, attempts-1))
	}
	return nil
}

func terminateContainer(containerName string, graceful bool) error {
	var stopErr error
	if graceful {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		output, err := exec.CommandContext(ctx, "docker", "stop", "--time", "10", containerName).CombinedOutput()
		cancel()
		if err != nil && !containerMissing(output) {
			stopErr = fmt.Errorf("docker stop %s: %s: %w", containerName, strings.TrimSpace(string(output)), err)
		}
	}
	if cleanupErr := cleanupContainer(containerName); cleanupErr != nil {
		if stopErr != nil {
			return fmt.Errorf("%v; force cleanup: %w", stopErr, cleanupErr)
		}
		return cleanupErr
	}
	return nil
}

func waitDockerCommand(cmd *exec.Cmd, done <-chan error) error {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	timer.Reset(5 * time.Second)
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return errors.New("docker client did not exit after container cleanup")
	}
}

func cleanupContainer(containerName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "rm", "-f", containerName).CombinedOutput()
	if err != nil {
		if containerMissing(output) {
			return nil
		}
		return fmt.Errorf("docker rm -f %s: %s: %w", containerName, strings.TrimSpace(string(output)), err)
	}
	output, err = exec.CommandContext(ctx, "docker", "inspect", containerName).CombinedOutput()
	if err == nil {
		return fmt.Errorf("container %s still exists after docker rm -f", containerName)
	}
	if containerMissing(output) {
		return nil
	}
	return fmt.Errorf("cannot confirm removal of %s: %s: %w", containerName, strings.TrimSpace(string(output)), err)
}

func containerMissing(output []byte) bool {
	message := strings.ToLower(string(output))
	return strings.Contains(message, "no such container") || strings.Contains(message, "no such object")
}

type liveJobLog struct {
	mu     sync.Mutex
	output []byte
	full   io.Writer
}

func (w *liveJobLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.full != nil {
		if _, err := w.full.Write(p); err != nil {
			return 0, err
		}
	}
	w.output = append(w.output, p...)
	if len(w.output) > 64<<10 {
		w.output = append([]byte(nil), w.output[len(w.output)-(64<<10):]...)
	}
	return len(p), nil
}

func (w *liveJobLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.ToValidUTF8(string(w.output), "�")
}

func (a *Agent) uploadLiveLog(parent context.Context, jobID, attemptToken, output string) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	_, err := a.doContext(ctx, http.MethodPost, "/v1/jobs/"+jobID+"/logs?node_id="+a.cfg.ID, model.JobLogUpdate{Output: output}, nil, a.jobHeaders(attemptToken))
	return err
}

func appendOutput(output, line string) string {
	if strings.TrimSpace(output) == "" {
		return line
	}
	return strings.TrimSpace(output) + "\n" + line
}

func archiveArtifacts(dir string) (string, error) {
	hasFiles := false
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			hasFiles = true
		}
		return nil
	})
	if err != nil || !hasFiles {
		return "", err
	}
	bundle := filepath.Join(filepath.Dir(dir), "artifacts.tar.gz")
	file, err := os.Create(bundle)
	if err != nil {
		return "", err
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	err = filepath.Walk(dir, func(filePath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == dir || !info.Mode().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, filePath)
		if relErr != nil {
			return relErr
		}
		header, headerErr := tar.FileInfoHeader(info, "")
		if headerErr != nil {
			return headerErr
		}
		header.Name = filepath.ToSlash(rel)
		if headerErr = tarWriter.WriteHeader(header); headerErr != nil {
			return headerErr
		}
		input, openErr := os.Open(filePath)
		if openErr != nil {
			return openErr
		}
		_, copyErr := io.Copy(tarWriter, input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if closeErr := tarWriter.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gzipWriter.Close(); err == nil {
		err = closeErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(bundle)
		return "", err
	}
	return bundle, nil
}
