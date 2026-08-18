package edition

// Descriptor is returned to the UI and is also the stable extension contract
// used by commercial builds. Keep it data-only so community and commercial
// binaries can share the same frontend.
type Descriptor struct {
	Name       string          `json:"name"`
	LicensedTo string          `json:"licensed_to,omitempty"`
	ExpiresAt  string          `json:"expires_at,omitempty"`
	AgentImage string          `json:"agent_image,omitempty"`
	PublicURL  string          `json:"public_url,omitempty"`
	Features   map[string]bool `json:"features"`
}

func Community() Descriptor {
	return Descriptor{
		Name:       "community",
		AgentImage: "gpuflow:latest",
		Features: map[string]bool{
			"basic_scheduler": true,
			"cost_analytics":  false,
			"advanced_policy": false,
			"alerts":          false,
			"rbac":            false,
			"audit_log":       false,
			"offline_license": false,
		},
	}
}
