package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"gpuflow/internal/model"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrNodeBusy    = errors.New("node has an assigned or running job")
	ErrJobActive   = errors.New("active job must be canceled before deletion")
	ErrPersistence = errors.New("persist state")
)

type snapshot struct {
	Jobs       map[string]*model.Job
	Nodes      map[string]*model.Node
	TaskImages map[string]*model.TaskImage
}

type Store struct {
	mu    sync.Mutex
	db    *sql.DB
	state snapshot
}

type TaskImageStore interface {
	SaveTaskImage(model.TaskImage) error
	ListTaskImages() ([]model.TaskImage, error)
	DeleteTaskImage(string) error
}

// NewMemory creates a non-persistent store for isolated unit tests. Production
// composition always uses OpenMySQLStateStore.
func NewMemory() *Store {
	return &Store{state: snapshot{Jobs: map[string]*model.Job{}, Nodes: map[string]*model.Node{}, TaskImages: map[string]*model.TaskImage{}}}
}

func (s *Store) SaveTaskImage(image model.TaskImage) error {
	if s.db != nil {
		if err := (&MySQLTaskImageStore{db: s.db}).SaveTaskImage(image); err != nil {
			return fmt.Errorf("%w: %v", ErrPersistence, err)
		}
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before := cloneSnapshot(s.state)
	copy := image
	s.state.TaskImages[image.ID] = &copy
	return s.commitLocked(before)
}

func (s *Store) ListTaskImages() ([]model.TaskImage, error) {
	if s.db != nil {
		return (&MySQLTaskImageStore{db: s.db}).ListTaskImages()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]model.TaskImage, 0, len(s.state.TaskImages))
	for _, image := range s.state.TaskImages {
		result = append(result, *image)
	}
	return result, nil
}

func (s *Store) DeleteTaskImage(id string) error {
	if s.db != nil {
		if err := (&MySQLTaskImageStore{db: s.db}).DeleteTaskImage(id); err != nil {
			if errors.Is(err, ErrNotFound) {
				return err
			}
			return fmt.Errorf("%w: %v", ErrPersistence, err)
		}
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.TaskImages[id]; !ok {
		return ErrNotFound
	}
	before := cloneSnapshot(s.state)
	delete(s.state.TaskImages, id)
	return s.commitLocked(before)
}

func (s *Store) commitLocked(before snapshot) error {
	if s.db == nil {
		return nil
	}
	err := s.saveMySQLChangesLocked(before)
	if err != nil {
		s.state = before
		if errors.Is(err, ErrPersistence) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrPersistence, err)
	}
	return nil
}

func cloneSnapshot(source snapshot) snapshot {
	result := snapshot{Jobs: make(map[string]*model.Job, len(source.Jobs)), Nodes: make(map[string]*model.Node, len(source.Nodes)), TaskImages: make(map[string]*model.TaskImage, len(source.TaskImages))}
	for id, job := range source.Jobs {
		copy := *job
		copy.Command = append([]string(nil), job.Command...)
		copy.Environment = cloneStringMap(job.Environment)
		copy.Requirements.GPUModels = append([]string(nil), job.Requirements.GPUModels...)
		copy.Requirements.Providers = append([]string(nil), job.Requirements.Providers...)
		copy.Requirements.Pools = append([]string(nil), job.Requirements.Pools...)
		copy.Requirements.Labels = cloneStringMap(job.Requirements.Labels)
		result.Jobs[id] = &copy
	}
	for id, node := range source.Nodes {
		copy := *node
		copy.Labels = cloneStringMap(node.Labels)
		result.Nodes[id] = &copy
	}
	for id, image := range source.TaskImages {
		copy := *image
		result.TaskImages[id] = &copy
	}
	return result
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
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
	before := cloneSnapshot(s.state)
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
	if err := s.commitLocked(before); err != nil {
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
	return status == model.JobSucceeded || status == model.JobFailed || status == model.JobCanceled || status == model.JobDeleting
}

func (s *Store) CancelJob(id string) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.state.Jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	before := cloneSnapshot(s.state)
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
	if err := s.commitLocked(before); err != nil {
		return nil, err
	}
	copy := *j
	return &copy, nil
}

func (s *Store) DeleteJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateJobDeletionLocked(id); err != nil {
		return err
	}
	before := cloneSnapshot(s.state)
	delete(s.state.Jobs, id)
	return s.commitLocked(before)
}

// BeginJobDeletion durably marks a terminal job before its artifacts are
// removed. A failed artifact or final state deletion can then be retried
// without returning the job to a runnable state.
func (s *Store) BeginJobDeletion(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.state.Jobs[id]
	if !ok {
		return ErrNotFound
	}
	if j.Status == model.JobDeleting {
		return nil
	}
	if !terminal(j.Status) {
		return ErrJobActive
	}
	before := cloneSnapshot(s.state)
	j.Status = model.JobDeleting
	j.UpdatedAt = time.Now().UTC()
	return s.commitLocked(before)
}

func (s *Store) ValidateJobDeletion(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.validateJobDeletionLocked(id)
}

func (s *Store) validateJobDeletionLocked(id string) error {
	j, ok := s.state.Jobs[id]
	if !ok {
		return ErrNotFound
	}
	if !terminal(j.Status) {
		return ErrJobActive
	}
	return nil
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

type NodeQuery struct {
	Search         string
	Page, PageSize int
}

type NodePage struct {
	Items      []*model.Node `json:"items"`
	Total      int           `json:"total"`
	Page       int           `json:"page"`
	PageSize   int           `json:"page_size"`
	TotalPages int           `json:"total_pages"`
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
	total := len(filtered)
	page, pageSize, start, end, pages := pageBounds(q.Page, q.PageSize, total)
	q.Page, q.PageSize = page, pageSize
	return JobPage{Items: filtered[start:end], Total: total, Page: q.Page, PageSize: q.PageSize, TotalPages: pages}
}

func pageBounds(page, pageSize, total int) (int, int, int, int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	pages := (total + pageSize - 1) / pageSize
	return page, pageSize, start, end, pages
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
	before := cloneSnapshot(s.state)
	now := time.Now().UTC()
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
		if in.CurrentJob != "" {
			job := s.state.Jobs[in.CurrentJob]
			switch {
			case job == nil || terminal(job.Status):
				in.Busy, in.CurrentJob = false, ""
			case job.Status == model.JobRunning:
				if job.Attempts > job.MaxRetries {
					job.Status, job.FinishedAt = model.JobFailed, &now
					job.Error = "agent restarted after interruption; retry budget exhausted"
					job.UpdatedAt = now
					in.Busy, in.CurrentJob = false, ""
				} else {
					// Registration means the agent process restarted. Re-run the
					// interrupted work as another attempt within its retry budget.
					job.Status = model.JobAssigned
					job.Attempts++
					job.Recoveries++
					job.StartedAt, job.FinishedAt = nil, nil
					job.Output, job.Error, job.UpdatedAt = "", "", now
				}
			}
		}
	} else {
		in.Busy, in.CurrentJob = false, ""
	}
	in.LastHeartbeat = now
	s.state.Nodes[in.ID] = &in
	if err := s.commitLocked(before); err != nil {
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
	before := cloneSnapshot(s.state)
	n.LastHeartbeat = time.Now().UTC()
	return s.commitLocked(before)
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

func (s *Store) QueryNodes(q NodeQuery) NodePage {
	nodes := s.ListNodes()
	search := strings.ToLower(strings.TrimSpace(q.Search))
	filtered := make([]*model.Node, 0, len(nodes))
	for _, node := range nodes {
		if search != "" && !strings.Contains(strings.ToLower(node.ID+" "+node.Name+" "+node.Provider+" "+node.Pool+" "+node.GPUModel), search) {
			continue
		}
		filtered = append(filtered, node)
	}
	page, pageSize, start, end, pages := pageBounds(q.Page, q.PageSize, len(filtered))
	q.Page, q.PageSize = page, pageSize
	return NodePage{Items: filtered[start:end], Total: len(filtered), Page: q.Page, PageSize: q.PageSize, TotalPages: pages}
}

func (s *Store) HasActiveJobForImage(image string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.state.Jobs {
		if job.Image == image && !terminal(job.Status) {
			return true
		}
	}
	return false
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
	before := cloneSnapshot(s.state)
	delete(s.state.Nodes, id)
	return s.commitLocked(before)
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
	before := cloneSnapshot(s.state)
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
		if err := s.commitLocked(before); err != nil {
			return err
		}
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
		before := cloneSnapshot(s.state)
		n.Busy, n.CurrentJob = false, ""
		if err := s.commitLocked(before); err != nil {
			return nil, err
		}
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
	before := cloneSnapshot(s.state)
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
	if err := s.commitLocked(before); err != nil {
		return nil, err
	}
	copy := *j
	return &copy, nil
}
