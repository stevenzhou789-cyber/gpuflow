package platform

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"gpuflow/internal/api"
	"gpuflow/internal/artifact"
	"gpuflow/internal/store"
	"gpuflow/pkg/edition"
)

// ArtifactConfig is the public S3-compatible storage contract used by
// distributions that compose GPUFlow without importing internal packages.
type ArtifactConfig struct {
	Endpoint, AccessKey, SecretKey, Bucket, Region string
	UseSSL                                         bool
}

// ImagePublisher is invoked after a task image is built and returns the image
// reference that remote agents should use.
type ImagePublisher interface {
	Publish(context.Context, io.Writer, string) (string, error)
}

type Config struct {
	MySQLDSN       string
	Token          string
	Descriptor     edition.Descriptor
	Artifacts      ArtifactConfig
	ImagePublisher ImagePublisher
}

// SchedulingLimits is the live scheduling-capacity policy. Zero node or GPU
// limits mean unlimited, matching edition.Descriptor semantics.
type SchedulingLimits struct {
	MaxNodes          int
	MaxGPUs           int
	ExpiresAt         string
	AcceleratorLimits map[string]edition.AcceleratorLimit
}

// SchedulingController is the stable control-plane contract for distributions
// that refresh scheduling policy at runtime (for example, after renewing a
// License). Implementations apply all fields atomically and never interrupt
// active work.
type SchedulingController interface {
	UpdateSchedulingLimits(SchedulingLimits) error
}

// Runtime owns the composed HTTP handler and its supported live controls.
// Callers should retain Runtime instead of asserting private methods on the
// returned http.Handler.
type Runtime struct {
	handler http.Handler
	state   *store.Store
}

func (r *Runtime) Handler() http.Handler { return r.handler }

// UpdateSchedulingLimits validates and atomically installs live scheduling
// limits. A malformed update is rejected without changing the existing policy.
func (r *Runtime) UpdateSchedulingLimits(limits SchedulingLimits) error {
	if err := validateSchedulingLimits(limits); err != nil {
		return err
	}
	r.state.SetCommercialLimits(limits.MaxNodes, limits.MaxGPUs, limits.ExpiresAt, limits.AcceleratorLimits)
	return nil
}

func validateSchedulingLimits(limits SchedulingLimits) error {
	if limits.MaxNodes < 0 || limits.MaxGPUs < 0 {
		return errors.New("scheduling capacity cannot be negative")
	}
	if _, err := edition.ParseExpiration(limits.ExpiresAt); err != nil {
		return err
	}
	for vendor, limit := range limits.AcceleratorLimits {
		vendor = strings.ToLower(strings.TrimSpace(vendor))
		if (vendor != "nvidia" && vendor != "huawei") || limit.MaxNodes < 1 || limit.MaxDevices < 1 {
			return errors.New("accelerator limits require nvidia or huawei with positive node and device quotas")
		}
	}
	return nil
}

// NewHandler is the supported composition point for Community and Enterprise
// distributions. Enterprise code can add middleware without importing internal packages.
func NewHandler(mysqlDSN, token string, descriptor edition.Descriptor, artifactConfig artifact.Config) (http.Handler, error) {
	return NewHandlerWithConfig(Config{
		MySQLDSN: mysqlDSN, Token: token, Descriptor: descriptor,
		Artifacts: ArtifactConfig{
			Endpoint: artifactConfig.Endpoint, AccessKey: artifactConfig.AccessKey,
			SecretKey: artifactConfig.SecretKey, Bucket: artifactConfig.Bucket,
			Region: artifactConfig.Region, UseSSL: artifactConfig.UseSSL,
		},
	})
}

// NewHandlerWithConfig is the stable composition point for Enterprise and
// other distributions. Community continues to use NewHandler unchanged.
func NewHandlerWithConfig(cfg Config) (http.Handler, error) {
	runtime, err := NewRuntimeWithConfig(cfg)
	if err != nil {
		return nil, err
	}
	return runtime.Handler(), nil
}

// NewRuntimeWithConfig composes GPUFlow and returns an explicit Runtime for
// distributions that need supported live scheduling controls. Its Handler has
// exactly the same routes and default FIFO behavior as NewHandlerWithConfig.
func NewRuntimeWithConfig(cfg Config) (*Runtime, error) {
	if strings.TrimSpace(cfg.MySQLDSN) == "" {
		return nil, errors.New("GPUFLOW_MYSQL_DSN is required")
	}
	if strings.TrimSpace(cfg.Artifacts.Endpoint) == "" {
		return nil, errors.New("GPUFLOW_S3_ENDPOINT is required")
	}
	if cfg.Descriptor.SchemaVersion != edition.CapabilitiesSchemaVersion {
		return nil, errors.New("control-plane capabilities schema does not match this GPUFlow release")
	}
	if cfg.Descriptor.Features[edition.FeatureNodeHealth] && strings.TrimSpace(cfg.Descriptor.ProbeImage) == "" {
		return nil, errors.New("GPUFLOW_PROBE_IMAGE is required when node health is enabled")
	}
	if err := validateSchedulingLimits(SchedulingLimits{
		MaxNodes: cfg.Descriptor.MaxNodes, MaxGPUs: cfg.Descriptor.MaxGPUs,
		ExpiresAt: cfg.Descriptor.ExpiresAt, AcceleratorLimits: cfg.Descriptor.AcceleratorLimits,
	}); err != nil {
		return nil, err
	}
	state, err := store.OpenMySQLStateStore(cfg.MySQLDSN)
	if err != nil {
		return nil, err
	}
	artifacts, err := artifact.Open(artifact.Config{
		Endpoint: cfg.Artifacts.Endpoint, AccessKey: cfg.Artifacts.AccessKey,
		SecretKey: cfg.Artifacts.SecretKey, Bucket: cfg.Artifacts.Bucket,
		Region: cfg.Artifacts.Region, UseSSL: cfg.Artifacts.UseSSL,
	})
	if err != nil {
		return nil, err
	}
	if !artifacts.Enabled() {
		return nil, errors.New("MinIO/S3 artifact storage is required")
	}
	handler := api.NewWithStoresAndPublisher(state, state, artifacts, cfg.Token, cfg.Descriptor, cfg.ImagePublisher).Handler()
	return &Runtime{handler: handler, state: state}, nil
}
