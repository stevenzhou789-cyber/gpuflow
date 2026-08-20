package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuflow/internal/client"
	"gpuflow/internal/model"
)

type Config struct {
	Server, Token, ID, Name, Provider, Pool, GPUModel, Executor, ArtifactDir string
	GPUCount, VRAMGB                                                         int
	HourlyPrice                                                              float64
	PollInterval, HeartbeatInterval, ArtifactUploadTimeout                   time.Duration
}
type Agent struct {
	cfg    Config
	client *client.Client
}

var errJobCanceled = errors.New("job canceled")

func New(cfg Config) *Agent {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 3 * time.Second
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 10 * time.Second
	}
	if cfg.ArtifactUploadTimeout == 0 {
		cfg.ArtifactUploadTimeout = 30 * time.Minute
	}
	return &Agent{cfg: cfg, client: client.New(cfg.Server, cfg.Token)}
}

func (a *Agent) Run(ctx context.Context) error {
	n := model.Node{ID: a.cfg.ID, Name: a.cfg.Name, Provider: a.cfg.Provider, Pool: a.cfg.Pool, GPUModel: a.cfg.GPUModel, GPUCount: a.cfg.GPUCount, VRAMGB: a.cfg.VRAMGB, HourlyPrice: a.cfg.HourlyPrice}
	if _, err := a.client.Do(http.MethodPost, "/v1/nodes/register", n, &n); err != nil {
		return fmt.Errorf("register node: %w", err)
	}
	a.cfg.ID = n.ID
	workers := a.cfg.GPUCount
	if workers < 1 {
		workers = 1
	}
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func() { defer group.Done(); a.runWorker(ctx) }()
	}
	<-ctx.Done()
	group.Wait()
	return ctx.Err()
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
	if _, err := a.client.Do(http.MethodPost, "/v1/nodes/"+a.cfg.ID+"/heartbeat", nil, nil); err != nil {
		return err
	}
	var job model.Job
	status, err := a.client.Do(http.MethodPost, "/v1/nodes/"+a.cfg.ID+"/next", nil, &job)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent {
		return nil
	}
	if job.Status == model.JobCanceling {
		_, err = a.client.Do(http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id="+a.cfg.ID, model.JobUpdate{Status: model.JobCanceled}, nil)
		return err
	}
	_, err = a.client.Do(http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id="+a.cfg.ID, model.JobUpdate{Status: model.JobRunning}, nil)
	if err != nil {
		return err
	}
	if a.cfg.ArtifactDir != "" {
		if err := os.MkdirAll(a.cfg.ArtifactDir, 0o755); err != nil {
			return fmt.Errorf("create artifact work directory: %w", err)
		}
	}
	artifactDir, err := os.MkdirTemp(a.cfg.ArtifactDir, "gpuflow-artifacts-"+job.ID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(artifactDir)
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		a.heartbeatLoop(heartbeatCtx)
	}()
	defer func() {
		stopHeartbeat()
		<-heartbeatDone
	}()
	output, runErr := a.execute(ctx, &job, artifactDir)
	if errors.Is(runErr, errJobCanceled) {
		_, err = a.client.Do(http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id="+a.cfg.ID, model.JobUpdate{Status: model.JobCanceled, Output: output}, nil)
		return err
	}
	var latest model.Job
	if _, statusErr := a.client.Do(http.MethodGet, "/v1/jobs/"+job.ID, nil, &latest); statusErr == nil && latest.Status == model.JobCanceling {
		_, err = a.client.Do(http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id="+a.cfg.ID, model.JobUpdate{Status: model.JobCanceled, Output: output}, nil)
		return err
	}
	if bundle, bundleErr := archiveArtifacts(artifactDir); bundleErr != nil {
		output = appendOutput(output, "artifact packaging warning: "+bundleErr.Error())
	} else if bundle != "" {
		defer os.Remove(bundle)
		if uploadErr := a.uploadArtifact(ctx, job.ID, bundle); uploadErr != nil {
			output = appendOutput(output, "artifact upload warning: "+uploadErr.Error())
		} else {
			output = appendOutput(output, "artifact uploaded: artifacts.tar.gz")
		}
	}
	if _, statusErr := a.client.Do(http.MethodGet, "/v1/jobs/"+job.ID, nil, &latest); statusErr == nil && latest.Status == model.JobCanceling {
		_, err = a.client.Do(http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id="+a.cfg.ID, model.JobUpdate{Status: model.JobCanceled, Output: output}, nil)
		return err
	}
	update := model.JobUpdate{Status: model.JobSucceeded, Output: output}
	if runErr != nil {
		update.Status = model.JobFailed
		update.Error = runErr.Error()
	}
	_, err = a.client.Do(http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id="+a.cfg.ID, update, nil)
	return err
}

func (a *Agent) uploadArtifact(parent context.Context, jobID, bundle string) error {
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
				if _, err := a.client.DoContext(ctx, http.MethodGet, "/v1/jobs/"+jobID, nil, &latest); err == nil && (latest.Status == model.JobCanceling || latest.Status == model.JobCanceled) {
					cancel()
					return
				}
			}
		}
	}()
	_, err := a.client.UploadArtifactContext(ctx, "/v1/jobs/"+jobID+"/artifacts?node_id="+a.cfg.ID, bundle)
	cancel()
	<-monitorDone
	return err
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = a.client.DoContext(ctx, http.MethodPost, "/v1/nodes/"+a.cfg.ID+"/heartbeat", nil, nil)
		}
	}
}

func (a *Agent) execute(parent context.Context, job *model.Job, artifactDir string) (string, error) {
	if a.cfg.Executor == "mock" {
		time.Sleep(250 * time.Millisecond)
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
	containerName := "gpuflow-job-" + job.ID
	// A previous agent process may have exited while Docker kept the job
	// container alive. Remove it before rerunning the recovered attempt.
	_ = exec.Command("docker", "rm", "-f", containerName).Run()
	args = append([]string{"run", "--name", containerName}, args[1:]...)
	cmd := exec.Command("docker", args...)
	output := &liveJobLog{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("docker run failed: %w", err)
	}
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
			_ = exec.Command("docker", "stop", "--time", "10", containerName).Run()
			err = <-done
			goto finished
		case <-ticker.C:
			current := output.String()
			if current != lastUploaded && time.Since(lastUpload) >= 2*time.Second {
				if uploadErr := a.uploadLiveLog(parent, job.ID, current); uploadErr != nil {
					fmt.Printf("agent log upload warning: %v\n", uploadErr)
				} else {
					lastUploaded, lastUpload = current, time.Now()
				}
			}
			var latest model.Job
			if _, pollErr := a.client.Do(http.MethodGet, "/v1/jobs/"+job.ID, nil, &latest); pollErr == nil && (latest.Status == model.JobCanceling || latest.Status == model.JobCanceled) {
				_ = exec.Command("docker", "stop", "--time", "10", containerName).Run()
				_ = <-done
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

type liveJobLog struct {
	mu     sync.Mutex
	output []byte
}

func (w *liveJobLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
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

func (a *Agent) uploadLiveLog(parent context.Context, jobID, output string) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	_, err := a.client.DoContext(ctx, http.MethodPost, "/v1/jobs/"+jobID+"/logs?node_id="+a.cfg.ID, model.JobLogUpdate{Output: output}, nil)
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
