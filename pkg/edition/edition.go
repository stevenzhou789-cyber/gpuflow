package edition

import (
	"fmt"
	"strings"
	"time"
)

const CapabilitiesSchemaVersion = 2

const (
	FeatureBasicScheduler            = "basic_scheduler"
	FeatureGPUGranularScheduling     = "gpu_granular_scheduling"
	FeatureCostAnalytics             = "cost_analytics"
	FeatureAdvancedPolicy            = "advanced_policy"
	FeatureAlerts                    = "alerts"
	FeatureRBAC                      = "rbac"
	FeatureAuditLog                  = "audit_log"
	FeatureOfflineLicense            = "offline_license"
	FeatureAgentBootstrap            = "agent_bootstrap"
	FeatureManagedRegistry           = "managed_registry"
	FeaturePerGPUInventory           = "per_gpu_inventory"
	FeatureNodeHealth                = "node_health"
	FeatureHeterogeneousAccelerators = "heterogeneous_accelerators"
)

// Descriptor is returned to the UI and is also the stable extension contract
// used by Enterprise builds. Keep it data-only so Community and Enterprise
// binaries can share the same frontend.
type Descriptor struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	LicensedTo    string `json:"licensed_to,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	AgentImage    string `json:"agent_image,omitempty"`
	ProbeImage    string `json:"probe_image,omitempty"`
	AgentBinary   string `json:"agent_binary,omitempty"`
	PublicURL     string `json:"public_url,omitempty"`
	MaxNodes      int    `json:"max_nodes,omitempty"`
	MaxGPUs       int    `json:"max_gpus,omitempty"`
	// AcceleratorLimits are signed Enterprise quotas keyed by accelerator vendor.
	// They are omitted from the public Agent capability projection.
	AcceleratorLimits map[string]AcceleratorLimit `json:"accelerator_limits,omitempty"`
	// MaxCPUCores is retained for compatibility with older enterprise integrations.
	// CPU capacity is no longer licensed or enforced.
	MaxCPUCores int             `json:"max_cpu_cores,omitempty"`
	Features    map[string]bool `json:"features"`
}

type AcceleratorLimit struct {
	MaxNodes   int `json:"max_nodes"`
	MaxDevices int `json:"max_devices"`
}

func ParseExpiration(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	// Date-only licenses remain supported for compatibility and expire at the
	// end of that UTC date. New commercial licenses should use RFC3339.
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed.Add(24*time.Hour - time.Nanosecond), nil
	}
	return time.Time{}, fmt.Errorf("expires_at must be RFC3339 or YYYY-MM-DD (UTC)")
}

func (d Descriptor) Expired(now time.Time) bool {
	expiresAt, err := ParseExpiration(d.ExpiresAt)
	return err == nil && !expiresAt.IsZero() && !now.Before(expiresAt)
}

// Public returns the capability projection safe for unauthenticated agents.
// Commercial identity, expiry, and capacity limits remain available only to
// authenticated control-plane clients.
func (d Descriptor) Public() Descriptor {
	features := make(map[string]bool, len(d.Features))
	for name, enabled := range d.Features {
		features[name] = enabled
	}
	return Descriptor{
		SchemaVersion: d.SchemaVersion,
		Name:          d.Name,
		AgentImage:    d.AgentImage,
		ProbeImage:    d.ProbeImage,
		AgentBinary:   d.AgentBinary,
		PublicURL:     d.PublicURL,
		Features:      features,
	}
}

func Community() Descriptor {
	return Descriptor{
		SchemaVersion: CapabilitiesSchemaVersion,
		Name:          "community",
		AgentImage:    "gpuflow:local",
		ProbeImage:    "gpuflow-gpu-probe:local",
		AgentBinary:   "gpuflow.exe",
		Features: map[string]bool{
			FeatureBasicScheduler:            true,
			FeatureGPUGranularScheduling:     false,
			FeatureCostAnalytics:             false,
			FeatureAdvancedPolicy:            false,
			FeatureAlerts:                    false,
			FeatureRBAC:                      false,
			FeatureAuditLog:                  false,
			FeatureOfflineLicense:            false,
			FeatureAgentBootstrap:            false,
			FeatureManagedRegistry:           false,
			FeaturePerGPUInventory:           false,
			FeatureNodeHealth:                false,
			FeatureHeterogeneousAccelerators: false,
		},
	}
}
