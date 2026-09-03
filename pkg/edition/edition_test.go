package edition

import (
	"strings"
	"testing"
	"time"
)

func TestCommunityDeclaresCompleteCapabilityContract(t *testing.T) {
	descriptor := Community()
	if descriptor.SchemaVersion != CapabilitiesSchemaVersion || descriptor.AgentBinary == "" || descriptor.ProbeImage == "" {
		t.Fatalf("incomplete descriptor: %+v", descriptor)
	}
	if !strings.Contains(descriptor.ProbeImage, "@sha256:") {
		t.Fatalf("Community probe image must be remotely pullable and immutable: %q", descriptor.ProbeImage)
	}
	for _, feature := range []string{FeatureGPUGranularScheduling, FeatureAgentBootstrap, FeatureManagedRegistry, FeaturePerGPUInventory, FeatureNodeHealth, FeatureHeterogeneousAccelerators} {
		if _, exists := descriptor.Features[feature]; !exists {
			t.Fatalf("missing capability %q", feature)
		}
	}
}

func TestExpirationSupportsCompatibleDateAndRFC3339(t *testing.T) {
	if _, err := ParseExpiration("not-a-date"); err == nil {
		t.Fatal("invalid expiration was accepted")
	}
	if !(Descriptor{ExpiresAt: "2020-01-01"}).Expired(time.Now()) {
		t.Fatal("past UTC date was not expired")
	}
	expiresAt := time.Now().Add(-time.Minute).Format(time.RFC3339)
	if !(Descriptor{ExpiresAt: expiresAt}).Expired(time.Now()) {
		t.Fatal("past RFC3339 expiration was not expired")
	}
}

func TestPublicDescriptorOmitsCommercialLicenseDetails(t *testing.T) {
	descriptor := Community()
	descriptor.LicensedTo, descriptor.ExpiresAt = "customer", "2030-01-01T00:00:00+08:00"
	descriptor.MaxNodes, descriptor.MaxGPUs = 10, 80
	descriptor.AcceleratorLimits = map[string]AcceleratorLimit{"huawei": {MaxNodes: 2, MaxDevices: 16}}
	public := descriptor.Public()
	if public.LicensedTo != "" || public.ExpiresAt != "" || public.MaxNodes != 0 || public.MaxGPUs != 0 || len(public.AcceleratorLimits) != 0 || public.ProbeImage != descriptor.ProbeImage || !public.Features[FeatureBasicScheduler] {
		t.Fatalf("unsafe public descriptor: %+v", public)
	}
}
