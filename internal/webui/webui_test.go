package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedDashboardAndSPAFallback(t *testing.T) {
	for _, path := range []string{"/", "/jobs"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s returned %d", path, response.Code)
		}
		if !strings.Contains(response.Body.String(), "GPUFlow") {
			t.Fatalf("%s did not return the dashboard", path)
		}
	}
}

func TestEmbeddedConnectCommandUsesManagedRegistryWithoutExternalPull(t *testing.T) {
	assets, err := fs.Glob(content, "dist/assets/*.js")
	if err != nil || len(assets) != 1 {
		t.Fatalf("unexpected embedded JavaScript assets: %v, %v", assets, err)
	}
	bundle, err := content.ReadFile(assets[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(bundle)
	if strings.Contains(text, "docker pull") || strings.Contains(text, "-probe-image") {
		t.Fatal("connect command still requires a node-side external Probe pull")
	}
	if !strings.Contains(text, "/enterprise/v1/registry/credentials") || !strings.Contains(text, "--password-stdin") {
		t.Fatal("container Agent command does not bootstrap the managed Registry")
	}
}
