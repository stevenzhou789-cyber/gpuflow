package main

import (
	"context"
	"errors"
	"flag"
	"gpuflow/internal/agent"
	"time"
)

func runAgentHandoff(args []string) error {
	fs := flag.NewFlagSet("agent-handoff", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 5*time.Minute, "maximum wait for current attempts to finish")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *timeout <= 0 || *timeout > 24*time.Hour {
		return errors.New("agent-handoff [--timeout DURATION] quiesce|resume|ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	return agent.LocalControl(ctx, "/run/gpuflow-agent.sock", fs.Arg(0))
}
