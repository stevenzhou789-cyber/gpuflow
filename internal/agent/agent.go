package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gpuflow/internal/client"
	"gpuflow/internal/model"
)

type Config struct {
	Server, Token, ID, Name, Provider, Pool, GPUModel, Executor, ArtifactDir string
	GPUCount, VRAMGB                                                         int
	HourlyPrice                                                              float64
	PollInterval                                                             time.Duration
}
type Agent struct {
	cfg    Config
	client *client.Client
}

func New(cfg Config) *Agent {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 3 * time.Second
	}
	return &Agent{cfg: cfg, client: client.New(cfg.Server, cfg.Token)}
}

func (a *Agent) Run(ctx context.Context) error {
	n := model.Node{ID: a.cfg.ID, Name: a.cfg.Name, Provider: a.cfg.Provider, Pool: a.cfg.Pool, GPUModel: a.cfg.GPUModel, GPUCount: a.cfg.GPUCount, VRAMGB: a.cfg.VRAMGB, HourlyPrice: a.cfg.HourlyPrice}
	if _, err := a.client.Do(http.MethodPost, "/v1/nodes/register", n, &n); err != nil {
		return fmt.Errorf("register node: %w", err)
	}
	a.cfg.ID = n.ID
	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := a.tick(ctx); err != nil {
			fmt.Printf("agent warning: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
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
	output, runErr := a.execute(ctx, &job, artifactDir)
	if bundle, bundleErr := archiveArtifacts(artifactDir); bundleErr != nil {
		output = appendOutput(output, "artifact packaging warning: "+bundleErr.Error())
	} else if bundle != "" {
		defer os.Remove(bundle)
		if _, uploadErr := a.client.UploadArtifact("/v1/jobs/"+job.ID+"/artifacts?node_id="+a.cfg.ID, bundle); uploadErr != nil {
			output = appendOutput(output, "artifact upload warning: "+uploadErr.Error())
		} else {
			output = appendOutput(output, "artifact uploaded: artifacts.tar.gz")
		}
	}
	update := model.JobUpdate{Status: model.JobSucceeded, Output: output}
	if runErr != nil {
		update.Status = model.JobFailed
		update.Error = runErr.Error()
	}
	_, err = a.client.Do(http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id="+a.cfg.ID, update, nil)
	return err
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
	if job.Requirements.GPUCount > 0 {
		args = append(args, "--gpus", fmt.Sprintf("%d", job.Requirements.GPUCount))
	}
	for k, v := range job.Environment {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, job.Image)
	args = append(args, job.Command...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
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
