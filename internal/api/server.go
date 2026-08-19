package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpuflow/internal/artifact"
	"gpuflow/internal/model"
	"gpuflow/internal/store"
	"gpuflow/internal/webui"
	"gpuflow/pkg/edition"
)

type Server struct {
	store     *store.Store
	token     string
	mux       *http.ServeMux
	edition   edition.Descriptor
	images    *ImageBuilder
	artifacts artifact.Store
}

func New(s *store.Store, token string) *Server {
	return NewWithEdition(s, token, edition.Community())
}

func NewWithEdition(s *store.Store, token string, descriptor edition.Descriptor) *Server {
	return NewWithTaskImageStore(s, s, token, descriptor)
}

func NewWithTaskImageStore(s *store.Store, taskImages store.TaskImageStore, token string, descriptor edition.Descriptor) *Server {
	return NewWithStores(s, taskImages, artifact.Disabled(), token, descriptor)
}

func NewWithStores(s *store.Store, taskImages store.TaskImageStore, artifacts artifact.Store, token string, descriptor edition.Descriptor) *Server {
	server := &Server{store: s, token: token, mux: http.NewServeMux(), edition: descriptor, images: NewImageBuilder(taskImages), artifacts: artifacts}
	server.routes()
	return server
}

func (s *Server) Handler() http.Handler {
	return logging(s.auth(s.mux))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	s.mux.HandleFunc("GET /v1/capabilities", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.edition) })
	s.mux.HandleFunc("POST /v1/jobs", s.createJob)
	s.mux.HandleFunc("GET /v1/jobs", s.listJobs)
	s.mux.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	s.mux.HandleFunc("POST /v1/jobs/{id}/rerun", s.rerunJob)
	s.mux.HandleFunc("POST /v1/jobs/{id}/cancel", s.cancelJob)
	s.mux.HandleFunc("DELETE /v1/jobs/{id}", s.deleteJob)
	s.mux.HandleFunc("POST /v1/jobs/{id}/status", s.updateJob)
	s.mux.HandleFunc("POST /v1/jobs/{id}/artifacts", s.uploadArtifact)
	s.mux.HandleFunc("GET /v1/jobs/{id}/artifacts", s.listArtifacts)
	s.mux.HandleFunc("GET /v1/jobs/{id}/artifacts/{name}", s.downloadArtifact)
	s.mux.HandleFunc("POST /v1/nodes/register", s.registerNode)
	s.mux.HandleFunc("POST /v1/nodes/{id}/heartbeat", s.heartbeat)
	s.mux.HandleFunc("POST /v1/nodes/{id}/next", s.nextJob)
	s.mux.HandleFunc("GET /v1/nodes", s.listNodes)
	s.mux.HandleFunc("DELETE /v1/nodes/{id}", s.deleteNode)
	s.mux.HandleFunc("POST /v1/task-images/build", s.buildTaskImage)
	s.mux.HandleFunc("GET /v1/task-images", s.listTaskImages)
	s.mux.Handle("/", webui.Handler())
}

func (s *Server) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.artifacts.Enabled() {
		writeError(w, http.StatusServiceUnavailable, artifact.ErrDisabled.Error())
		return
	}
	job, err := s.store.GetJob(r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if nodeID := r.URL.Query().Get("node_id"); nodeID == "" || nodeID != job.AssignedNode {
		writeError(w, http.StatusForbidden, "artifact upload is only allowed from the assigned node")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer file.Close()
	name := filepath.Base(header.Filename)
	if err := s.artifacts.Put(r.Context(), job.ID, name, file, header.Size); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, artifact.Item{Name: name, Size: header.Size, LastModified: time.Now().UTC()})
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.GetJob(r.PathValue("id")); err != nil {
		handleStoreError(w, err)
		return
	}
	items, err := s.artifacts.List(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": s.artifacts.Enabled(), "items": items})
}

func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.GetJob(r.PathValue("id")); err != nil {
		handleStoreError(w, err)
		return
	}
	reader, item, err := s.artifacts.Open(r.Context(), r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": item.Name}))
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", fmt.Sprint(item.Size))
	_, _ = io.Copy(w, reader)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/v1/capabilities" || !strings.HasPrefix(r.URL.Path, "/v1/") || s.token == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			writeError(w, 401, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var in model.JobCreate
	if err := decode(w, r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Image) == "" {
		writeError(w, 400, "name and image are required")
		return
	}
	if in.MaxRetries < 0 || in.TimeoutSeconds < 0 || in.Requirements.GPUCount < 0 {
		writeError(w, 400, "numeric fields cannot be negative")
		return
	}
	j, err := s.store.CreateJob(in)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.scheduleBestEffort()
	writeJSON(w, 201, j)
}
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery == "" {
		writeJSON(w, 200, s.store.ListJobs())
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	writeJSON(w, 200, s.store.QueryJobs(store.JobQuery{Search: r.URL.Query().Get("q"), Status: r.URL.Query().Get("status"), Pool: r.URL.Query().Get("pool"), Node: r.URL.Query().Get("node"), Sort: r.URL.Query().Get("sort"), Order: r.URL.Query().Get("order"), Page: page, PageSize: pageSize}))
}
func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.GetJob(r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, 200, j)
}
func (s *Server) rerunJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.RerunJob(r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	s.scheduleBestEffort()
	writeJSON(w, http.StatusCreated, j)
}
func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.CancelJob(r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}
func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	if err := s.store.BeginJobDeletion(r.PathValue("id")); err != nil {
		handleStoreError(w, err)
		return
	}
	if r.URL.Query().Get("delete_artifacts") != "false" {
		if err := s.artifacts.Delete(r.Context(), r.PathValue("id")); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	if err := s.store.DeleteJob(r.PathValue("id")); err != nil {
		handleStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) registerNode(w http.ResponseWriter, r *http.Request) {
	var n model.Node
	if err := decode(w, r, &n); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	saved, err := s.store.RegisterNode(n)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.scheduleBestEffort()
	writeJSON(w, 200, saved)
}
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	if err := s.store.HeartbeatNode(r.PathValue("id")); err != nil {
		handleStoreError(w, err)
		return
	}
	s.scheduleBestEffort()
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
func (s *Server) listNodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, s.store.ListNodes())
}
func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteNode(r.PathValue("id")); err != nil {
		handleStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) nextJob(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Schedule(30 * time.Second); err != nil {
		writeError(w, http.StatusInternalServerError, "schedule jobs: "+err.Error())
		return
	}
	j, err := s.store.NextJob(r.PathValue("id"))
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if j == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, 200, j)
}
func (s *Server) updateJob(w http.ResponseWriter, r *http.Request) {
	var in model.JobUpdate
	if err := decode(w, r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	nodeID := r.URL.Query().Get("node_id")
	if nodeID == "" {
		writeError(w, 400, "node_id is required")
		return
	}
	j, err := s.store.UpdateJob(r.PathValue("id"), nodeID, in)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	s.scheduleBestEffort()
	writeJSON(w, 200, j)
}

func (s *Server) scheduleBestEffort() {
	if err := s.store.Schedule(30 * time.Second); err != nil {
		log.Printf("schedule jobs: %v", err)
	}
}
func handleStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrPersistence) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "not found")
		return
	}
	if errors.Is(err, store.ErrNodeBusy) {
		writeError(w, http.StatusConflict, "node has an assigned or running job")
		return
	}
	if errors.Is(err, store.ErrJobActive) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeError(w, 409, err.Error())
}
