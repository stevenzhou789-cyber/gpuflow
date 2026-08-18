package platform

import (
	"net/http"

	"gpuflow/internal/api"
	"gpuflow/internal/store"
	"gpuflow/pkg/edition"
)

// NewHandler is the supported composition point for Community and commercial
// distributions. Commercial code can add middleware without importing internal packages.
func NewHandler(statePath, token string, descriptor edition.Descriptor) (http.Handler, error) {
	state, err := store.Open(statePath)
	if err != nil {
		return nil, err
	}
	return api.NewWithEdition(state, token, descriptor).Handler(), nil
}
