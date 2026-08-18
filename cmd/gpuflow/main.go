package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"gpuflow/internal/agent"
	"gpuflow/internal/client"
	"gpuflow/internal/model"
	"gpuflow/pkg/edition"
	"gpuflow/pkg/platform"
)

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
func envInt(name string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(name))
	if err == nil {
		return v
	}
	return fallback
}
func envFloat(name string, fallback float64) float64 {
	v, err := strconv.ParseFloat(os.Getenv(name), 64)
	if err == nil {
		return v
	}
	return fallback
}
func usage() {
	fmt.Fprintln(os.Stderr, "Usage: gpuflow <server|agent|submit|jobs|nodes|get> [options]")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "server":
		err = runServer(os.Args[2:])
	case "agent":
		err = runAgent(os.Args[2:])
	case "submit":
		err = submit(os.Args[2:])
	case "jobs", "nodes", "get":
		err = query(os.Args[1], os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	addr := fs.String("addr", env("GPUFLOW_ADDR", ":8080"), "listen address")
	data := fs.String("data", env("GPUFLOW_DATA", "./data/state.json"), "state file")
	token := fs.String("token", env("GPUFLOW_TOKEN", ""), "bearer token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	descriptor := edition.Community()
	descriptor.AgentImage = env("GPUFLOW_AGENT_IMAGE", descriptor.AgentImage)
	handler, err := platform.NewHandler(*data, *token, descriptor)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-signalChan()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	fmt.Printf("gpuflow server listening on %s\n", *addr)
	err = srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	cfg := agent.Config{}
	fs.StringVar(&cfg.Server, "server", env("GPUFLOW_SERVER", "http://localhost:8080"), "control plane URL")
	fs.StringVar(&cfg.Token, "token", env("GPUFLOW_TOKEN", ""), "bearer token")
	fs.StringVar(&cfg.ID, "id", env("GPUFLOW_NODE_ID", ""), "stable node ID")
	fs.StringVar(&cfg.Name, "name", env("GPUFLOW_NODE_NAME", "local-agent"), "node name")
	fs.StringVar(&cfg.Provider, "provider", env("GPUFLOW_PROVIDER", "local"), "provider")
	fs.StringVar(&cfg.Pool, "pool", env("GPUFLOW_POOL", "default"), "pool")
	fs.StringVar(&cfg.GPUModel, "gpu-model", env("GPUFLOW_GPU_MODEL", "generic"), "GPU model")
	fs.IntVar(&cfg.GPUCount, "gpu-count", envInt("GPUFLOW_GPU_COUNT", 1), "GPU count")
	fs.IntVar(&cfg.VRAMGB, "vram", envInt("GPUFLOW_VRAM_GB", 24), "VRAM in GB")
	fs.Float64Var(&cfg.HourlyPrice, "hourly-price", envFloat("GPUFLOW_HOURLY_PRICE", 0), "estimated hourly price")
	fs.StringVar(&cfg.Executor, "executor", env("GPUFLOW_EXECUTOR", "docker"), "docker or mock")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := agent.New(cfg).Run(ctx)
	if err == context.Canceled {
		return nil
	}
	return err
}

func submit(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	file := fs.String("file", "", "job JSON file")
	server := fs.String("server", env("GPUFLOW_SERVER", "http://localhost:8080"), "control plane URL")
	token := fs.String("token", env("GPUFLOW_TOKEN", ""), "bearer token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("-file is required")
	}
	b, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var in model.JobCreate
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	var out model.Job
	_, err = client.New(*server, *token).Do(http.MethodPost, "/v1/jobs", in, &out)
	if err == nil {
		printJSON(out)
	}
	return err
}

func query(command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	server := fs.String("server", env("GPUFLOW_SERVER", "http://localhost:8080"), "control plane URL")
	token := fs.String("token", env("GPUFLOW_TOKEN", ""), "bearer token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := "/v1/" + command
	if command == "get" {
		if fs.NArg() != 1 {
			return fmt.Errorf("get requires a job ID")
		}
		path = "/v1/jobs/" + fs.Arg(0)
	}
	var out any
	_, err := client.New(*server, *token).Do(http.MethodGet, path, nil, &out)
	if err == nil {
		printJSON(out)
	}
	return err
}

func printJSON(v any) { b, _ := json.MarshalIndent(v, "", "  "); fmt.Println(string(b)) }
func signalChan() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	return ch
}
