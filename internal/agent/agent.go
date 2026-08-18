package agent

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"gpuflow/internal/client"
	"gpuflow/internal/model"
)

type Config struct {
	Server, Token, ID, Name, Provider, Pool, GPUModel, Executor string
	GPUCount, VRAMGB                                            int
	HourlyPrice                                                 float64
	PollInterval                                                time.Duration
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
	output, runErr := a.execute(ctx, &job)
	update := model.JobUpdate{Status: model.JobSucceeded, Output: output}
	if runErr != nil {
		update.Status = model.JobFailed
		update.Error = runErr.Error()
	}
	_, err = a.client.Do(http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id="+a.cfg.ID, update, nil)
	return err
}

func (a *Agent) execute(parent context.Context, job *model.Job) (string, error) {
	if a.cfg.Executor == "mock" {
		time.Sleep(250 * time.Millisecond)
		return "mock executor completed", nil
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(job.TimeoutSeconds)*time.Second)
	defer cancel()
	args := []string{"run", "--rm", "--label", "gpuflow.job=" + job.ID}
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
