package platform

import (
	"net/http"

	"gpuflow/internal/api"
	"gpuflow/internal/artifact"
	"gpuflow/internal/store"
	"gpuflow/pkg/edition"
)

// NewHandler is the supported composition point for Community and commercial
// distributions. Commercial code can add middleware without importing internal packages.
func NewHandler(statePath, mysqlDSN, token string, descriptor edition.Descriptor, artifactConfig artifact.Config) (http.Handler, error) {
	state, err := store.Open(statePath)
	if err != nil {
		return nil, err
	}
	var taskImages store.TaskImageStore = state
	if mysqlDSN != "" {
		mysqlStore, err := store.OpenMySQLTaskImageStore(mysqlDSN)
		if err != nil {
			return nil, err
		}
		legacyImages, err := state.ListTaskImages()
		if err != nil {
			return nil, err
		}
		for _, image := range legacyImages {
			if err := mysqlStore.SaveTaskImage(image); err != nil {
				return nil, err
			}
		}
		taskImages = mysqlStore
	}
	artifacts, err := artifact.Open(artifactConfig)
	if err != nil {
		return nil, err
	}
	return api.NewWithStores(state, taskImages, artifacts, token, descriptor).Handler(), nil
}
