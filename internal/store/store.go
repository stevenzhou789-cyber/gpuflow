package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gpuflow/internal/model"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrNodeBusy  = errors.New("node has an assigned or running job")
	ErrJobActive = errors.New("active job must be canceled before deletion")
)

type snapshot struct {
	Jobs       map[string]*model.Job       `json:"jobs"`
	Nodes      map[string]*model.Node      `json:"nodes"`
	TaskImages map[string]*model.TaskImage `json:"task_images,omitempty"`
}

type Store struct {
	mu    sync.Mutex
	path  string
	state snapshot
}

type TaskImageStore interface {
	SaveTaskImage(model.TaskImage) error
	ListTaskImages() ([]model.TaskImage, error)
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, state: snapshot{Jobs: map[string]*model.Job{}, Nodes: map[string]*model.Node{}, TaskImages: map[string]*model.TaskImage{}}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.state); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if s.state.Jobs == nil {
		s.state.Jobs = map[string]*model.Job{}
	}
	if s.state.Nodes == nil {
		s.state.Nodes = map[string]*model.Node{}
	}
	if s.state.TaskImages == nil {
		s.state.TaskImages = map[string]*model.TaskImage{}
	}
	return s, nil
}

func (s *Store) SaveTaskImage(image model.TaskImage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := image
	s.state.TaskImages[image.ID] = &copy
	return s.saveLocked()
}

func (s *Store) ListTaskImages() ([]model.TaskImage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]model.TaskImage, 0, len(s.state.TaskImages))
	for _, image := range s.state.TaskImages {
		result = append(result, *image)
	}
	return result, nil
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	_ = os.Remove(s.path)
	return os.Rename(tmp, s.path)
}

func newID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func (s *Store) CreateJob(in model.JobCreate) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createJobLocked(in, "")
}

func (s *Store) createJobLocked(in model.JobCreate, rerunOf string) (*model.Job, error) {
	now := time.Now().UTC()
	strategy := in.Strategy
	if strategy == "" {
		strategy = "lowest_cost"
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = 3600
	}
	j := &model.Job{ID: newID("job"), Name: in.Name, Image: in.Image, Command: in.Command,
		Environment: in.Environment, Requirements: in.Requirements, Strategy: strategy,
		TimeoutSeconds: in.TimeoutSeconds, MaxRetries: in.MaxRetries, Status: model.JobQueued,
		CreatedAt: now, UpdatedAt: now, RerunOf: rerunOf}
	s.state.Jobs[j.ID] = j
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	copy := *j
	return &copy, nil
}

func (s *Store) RerunJob(id string) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	original, ok := s.state.Jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	in := model.JobCreate{Name: original.Name, Image: original.Image, Command: original.Command, Environment: original.Environment, Requirements: original.Requirements, Strategy: original.Strategy, TimeoutSeconds: original.TimeoutSeconds, MaxRetries: original.MaxRetries}
	return s.createJobLocked(in, original.ID)
}

func terminal(status model.JobStatus) bool {
	return status == model.JobSucceeded || status == model.JobFailed || status == model.JobCanceled
}

func (s *Store) CancelJob(id string) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.state.Jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	switch j.Status {
	case model.JobQueued:
		j.Status, j.FinishedAt = model.JobCanceled, &now
	case model.JobAssigned, model.JobRunning:
		j.Status = model.JobCanceling
	case model.JobCanceling, model.JobCanceled:
		// Idempotent.
	default:
		return nil, errors.New("completed job cannot be canceled")
	}
	j.UpdatedAt = now
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	copy := *j
	return &copy, nil
}

func (s *Store) DeleteJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.state.Jobs[id]
	if !ok {
		return ErrNotFound
	}
	if !terminal(j.Status) {
		return ErrJobActive
	}
	delete(s.state.Jobs, id)
	return s.saveLocked()
}

type JobQuery struct {
	Search, Status, Pool, Node, Sort, Order string
	Page, PageSize                          int
}

type JobPage struct {
	Items      []*model.Job `json:"items"`
	Total      int          `json:"total"`
	Page       int          `json:"page"`
	PageSize   int          `json:"page_size"`
	TotalPages int          `json:"total_pages"`
}

func (s *Store) QueryJobs(q JobQuery) JobPage {
	jobs := s.ListJobs()
	filtered := make([]*model.Job, 0, len(jobs))
	for _, j := range jobs {
		search := strings.ToLower(q.Search)
		if search != "" && !strings.Contains(strings.ToLower(j.ID+" "+j.Name+" "+j.Image+" "+j.AssignedNode), search) {
			continue
		}
		if q.Status != "" && string(j.Status) != q.Status {
			continue
		}
		if q.Node != "" && j.AssignedNode != q.Node {
			continue
		}
		if q.Pool != "" && !containsFold(j.Requirements.Pools, q.Pool) {
			continue
		}
		filtered = append(filtered, j)
	}
	sort.SliceStable(filtered, func(i, k int) bool {
		a, b := filtered[i], filtered[k]
		var av, bv int64
		switch q.Sort {
		case "started_at":
			av, bv = timeValue(a.StartedAt).UnixNano(), timeValue(b.StartedAt).UnixNano()
		case "finished_at":
			av, bv = timeValue(a.FinishedAt).UnixNano(), timeValue(b.FinishedAt).UnixNano()
		case "duration":
			av, bv = int64(jobDuration(a)), int64(jobDuration(b))
		default:
			av, bv = a.CreatedAt.UnixNano(), b.CreatedAt.UnixNano()
		}
		if av == bv {
			return a.ID < b.ID
		}
		if strings.EqualFold(q.Order, "asc") {
			return av < bv
		}
		return av > bv
	})
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = 20
	}
	if q.PageSize > 100 {
		q.PageSize = 100
	}
	total := len(filtered)
	start := (q.Page - 1) * q.PageSize
	if start > total {
		start = total
	}
	end := start + q.PageSize
	if end > total {
		end = total
	}
	pages := (total + q.PageSize - 1) / q.PageSize
	return JobPage{Items: filtered[start:end], Total: total, Page: q.Page, PageSize: q.PageSize, TotalPages: pages}
}

func timeValue(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}
func jobDuration(j *model.Job) time.Duration {
	if j.StartedAt == nil {
		return 0
	}
	end := time.Now()
	if j.FinishedAt != nil {
		end = *j.FinishedAt
	}
	return end.Sub(*j.StartedAt)
}

func (s *Store) GetJob(id string) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.state.Jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	copy := *j
	return &copy, nil
}

func (s *Store) ListJobs() []*model.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*model.Job, 0, len(s.state.Jobs))
	for _, j := range s.state.Jobs {
		copy := *j
		result = append(result, &copy)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

func (s *Store) RegisterNode(in model.Node) (*model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.ID == "" {
		in.ID = newID("node")
	}
	if in.Name == "" {
		in.Name = in.ID
	}
	if in.Provider == "" {
		in.Provider = "local"
	}
	if in.Pool == "" {
		in.Pool = "default"
	}
	if old, ok := s.state.Nodes[in.ID]; ok {
		in.Busy, in.CurrentJob = old.Busy, old.CurrentJob
	} else {
		in.Busy, in.CurrentJob = false, ""
	}
	in.LastHeartbeat = time.Now().UTC()
	s.state.Nodes[in.ID] = &in
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	copy := in
	return &copy, nil
}

func (s *Store) HeartbeatNode(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.state.Nodes[id]
	if !ok {
		return ErrNotFound
	}
	n.LastHeartbeat = time.Now().UTC()
	return s.saveLocked()
}

func (s *Store) ListNodes() []*model.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*model.Node, 0, len(s.state.Nodes))
	for _, n := range s.state.Nodes {
		copy := *n
		result = append(result, &copy)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (s *Store) DeleteNode(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.state.Nodes[id]
	if !ok {
		return ErrNotFound
	}
	if node.Busy || node.CurrentJob != "" {
		return ErrNodeBusy
	}
	for _, job := range s.state.Jobs {
		if job.AssignedNode == id && (job.Status == model.JobAssigned || job.Status == model.JobRunning) {
			return ErrNodeBusy
		}
	}
	delete(s.state.Nodes, id)
	return s.saveLocked()
}

func containsFold(values []string, candidate string) bool {
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		if strings.EqualFold(value, candidate) {
			return true
		}
	}
	return false
}

func eligible(j *model.Job, n *model.Node, now time.Time, offlineAfter time.Duration) bool {
	if n.Busy || now.Sub(n.LastHeartbeat) > offlineAfter {
		return false
	}
	r := j.Requirements
	if n.GPUCount < r.GPUCount || n.VRAMGB < r.MinVRAMGB {
		return false
	}
	if r.MaxHourly > 0 && n.HourlyPrice > r.MaxHourly {
		return false
	}
	if !containsFold(r.GPUModels, n.GPUModel) || !containsFold(r.Providers, n.Provider) || !containsFold(r.Pools, n.Pool) {
		return false
	}
	for k, v := range r.Labels {
		if n.Labels[k] != v {
			return false
		}
	}
	return true
}

func betterNode(strategy string, a, b *model.Node) bool {
	if b == nil {
		return true
	}
	switch strategy {
	case "most_vram", "fastest":
		if a.VRAMGB != b.VRAMGB {
			return a.VRAMGB > b.VRAMGB
		}
		return a.HourlyPrice < b.HourlyPrice
	default:
		if a.HourlyPrice != b.HourlyPrice {
			return a.HourlyPrice < b.HourlyPrice
		}
		return a.VRAMGB > b.VRAMGB
	}
}

func (s *Store) Schedule(offlineAfter time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	jobs := make([]*model.Job, 0)
	for _, j := range s.state.Jobs {
		if j.Status == model.JobQueued {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	changed := false
	for _, j := range jobs {
		var best *model.Node
		for _, n := range s.state.Nodes {
			if eligible(j, n, now, offlineAfter) && betterNode(j.Strategy, n, best) {
				best = n
			}
		}
		if best == nil {
			continue
		}
		best.Busy, best.CurrentJob = true, j.ID
		j.Status, j.AssignedNode, j.UpdatedAt = model.JobAssigned, best.ID, now
		j.Attempts++
		changed = true
	}
	if changed {
		return s.saveLocked()
	}
	return nil
}

func (s *Store) NextJob(nodeID string) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.state.Nodes[nodeID]
	if !ok {
		return nil, ErrNotFound
	}
	if n.CurrentJob == "" {
		return nil, nil
	}
	j, ok := s.state.Jobs[n.CurrentJob]
	if !ok {
		n.Busy, n.CurrentJob = false, ""
		_ = s.saveLocked()
		return nil, nil
	}
	copy := *j
	return &copy, nil
}

func (s *Store) UpdateJob(id, nodeID string, in model.JobUpdate) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.state.Jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	if j.AssignedNode != nodeID {
		return nil, errors.New("job is not assigned to this node")
	}
	now := time.Now().UTC()
	switch in.Status {
	case model.JobRunning:
		if j.Status != model.JobAssigned {
			return nil, errors.New("job is not assigned")
		}
		j.Status, j.StartedAt = model.JobRunning, &now
	case model.JobSucceeded:
		if j.Status != model.JobRunning && j.Status != model.JobAssigned {
			return nil, errors.New("job is not running")
		}
		j.Status, j.FinishedAt = model.JobSucceeded, &now
		if n := s.state.Nodes[nodeID]; n != nil {
			n.Busy, n.CurrentJob = false, ""
		}
	case model.JobFailed:
		if n := s.state.Nodes[nodeID]; n != nil {
			n.Busy, n.CurrentJob = false, ""
		}
		j.FinishedAt = &now
		if j.Attempts <= j.MaxRetries {
			j.Status, j.AssignedNode = model.JobQueued, ""
			j.StartedAt, j.FinishedAt = nil, nil
		} else {
			j.Status = model.JobFailed
		}
	case model.JobCanceled:
		if j.Status != model.JobCanceling {
			return nil, errors.New("job is not canceling")
		}
		j.Status, j.FinishedAt = model.JobCanceled, &now
		if n := s.state.Nodes[nodeID]; n != nil {
			n.Busy, n.CurrentJob = false, ""
		}
	default:
		return nil, errors.New("unsupported status transition")
	}
	j.Output, j.Error, j.UpdatedAt = in.Output, in.Error, now
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	copy := *j
	return &copy, nil
}
