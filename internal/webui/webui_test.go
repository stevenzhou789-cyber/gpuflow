package webui

import (
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
