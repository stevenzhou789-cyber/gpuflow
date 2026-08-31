package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"gpuflow/internal/model"
)

var (
	ErrNotFound           = errors.New("not found")
	ErrNodeBusy           = errors.New("node has an assigned or running job")
	ErrJobActive          = errors.New("active job must be canceled before deletion")
	ErrPersistence        = errors.New("persist state")
	ErrInvalidResources   = errors.New("invalid resources")
	ErrLicenseCapacity    = errors.New("enterprise license capacity exceeded")
	ErrLicenseExpired     = errors.New("enterprise license expired; new capacity is disabled")
	ErrAgentSession       = errors.New("invalid agent session")
	ErrAgentSessionActive = errors.New("another agent session for this node is still active")
	ErrAttemptLease       = errors.New("invalid or expired job attempt lease")
	ErrNodeUnavailable    = errors.New("node is not eligible to start jobs")
)

const (
	agentSessionActiveFor = model.AgentSessionTTL
	jobLeaseDuration      = 30 * time.Second
	maxGPUsPerNodeOrJob   = 1024
	maxPersistedJobInt    = int64(1<<31 - 1)
	recoveryCleanupMarker = "[gpuflow:recovery-cleanup-before-failed]"
	recoveryRetryMarker   = "[gpuflow:recovery-cleanup-before-retry]"
)

type snapshot struct {
	Jobs       map[string]*model.Job
	Nodes      map[string]*model.Node
	TaskImages map[string]*model.TaskImage
}

type Store struct {
	mu                     sync.Mutex
	db                     *sql.DB
	state                  snapshot
	gpuGranularScheduling  bool
	maxNodes               int
	maxGPUs                int
	licenseExpiresAt       time.Time
	nodeHealthEnabled      bool
	nodeHealthTTL          time.Duration
	perGPUInventoryEnabled bool
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

func (s *Store) SetGPUGranularScheduling(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gpuGranularScheduling = enabled
	for id := range s.state.Nodes {
		s.refreshNodeUsageLocked(id)
	}
}

// SetSchedulingLimits applies enterprise limits to both freshly registered
// and already persisted nodes. Active work is never interrupted; nodes outside
// the current license capacity simply stop receiving new jobs.
func (s *Store) SetSchedulingLimits(maxNodes, maxGPUs int, expiresAt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxNodes, s.maxGPUs = maxNodes, maxGPUs
	s.licenseExpiresAt = time.Time{}
	if value := strings.TrimSpace(expiresAt); value != "" {
		if parsed, err := time.Parse(time.RFC3339, value); err == nil {
			s.licenseExpiresAt = parsed
		} else if parsed, err := time.Parse("2006-01-02", value); err == nil {
			s.licenseExpiresAt = parsed.Add(24*time.Hour - time.Nanosecond)
		} else {
			// Production composition rejects malformed licenses. Keep the
			// Store fail-closed for direct embedders as well.
			s.licenseExpiresAt = time.Unix(0, 0).UTC()
		}
	}
}

func (s *Store) SetNodeHealthPolicy(enabled bool, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodeHealthEnabled = enabled
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	s.nodeHealthTTL = ttl
}

func (s *Store) SetPerGPUInventory(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.perGPUInventoryEnabled = enabled
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
		copy.LeaseExpiresAt = cloneTime(job.LeaseExpiresAt)
		copy.AllocatedGPUs = append([]int(nil), job.AllocatedGPUs...)
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
		copy.LastHealthCheck = cloneTime(node.LastHealthCheck)
		copy.Labels = cloneStringMap(node.Labels)
		copy.ActiveJobs = append([]string(nil), node.ActiveJobs...)
		copy.Devices = append([]model.GPUDevice(nil), node.Devices...)
		result.Nodes[id] = &copy
	}
	for id, image := range source.TaskImages {
		copy := *image
		result.TaskImages[id] = &copy
	}
	return result
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
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

func newToken(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func (s *Store) CreateJob(in model.JobCreate) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createJobLocked(in, "")
}

func (s *Store) createJobLocked(in model.JobCreate, rerunOf string) (*model.Job, error) {
	if err := validateAndNormalizeJobCreate(&in); err != nil {
		return nil, err
	}
	before := cloneSnapshot(s.state)
	now := time.Now().UTC()
	for _, existing := range s.state.Jobs {
		if !now.After(existing.CreatedAt) {
			now = existing.CreatedAt.Add(time.Nanosecond)
		}
	}
	strategy := in.Strategy
	if strategy == "" {
		strategy = "lowest_cost"
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

func validateAndNormalizeJobCreate(in *model.JobCreate) error {
	if in.Requirements.GPUCount < 0 || in.Requirements.MinVRAMGB < 0 || in.Requirements.MaxHourly < 0 {
		return fmt.Errorf("%w: job resource requirements cannot be negative", ErrInvalidResources)
	}
	if in.Requirements.GPUCount > maxGPUsPerNodeOrJob {
		return fmt.Errorf("%w: gpu_count cannot exceed %d", ErrInvalidResources, maxGPUsPerNodeOrJob)
	}
	if math.IsNaN(in.Requirements.MaxHourly) || math.IsInf(in.Requirements.MaxHourly, 0) {
		return fmt.Errorf("%w: max_hourly_price must be finite", ErrInvalidResources)
	}
	if in.TimeoutSeconds < 0 || int64(in.TimeoutSeconds) > maxPersistedJobInt {
		return fmt.Errorf("%w: timeout_seconds must be between 0 and %d", ErrInvalidResources, maxPersistedJobInt)
	}
	if in.MaxRetries < 0 || int64(in.MaxRetries) > maxPersistedJobInt {
		return fmt.Errorf("%w: max_retries must be between 0 and %d", ErrInvalidResources, maxPersistedJobInt)
	}
	if in.Requirements.GPUCount == 0 {
		// CPU-only jobs are whole-node exclusive. GPU-only filters must not make
		// them ineligible for CPU nodes or nodes with a different GPU inventory.
		in.Requirements.MinVRAMGB = 0
		in.Requirements.GPUModels = nil
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = 3600
	}
	return nil
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
	nodeID := j.AssignedNode
	switch j.Status {
	case model.JobQueued:
		j.Status, j.FinishedAt = model.JobCanceled, &now
		clearJobAssignment(j)
	case model.JobAssigned:
		// An assigned job has not crossed the guarded ASSIGNED -> RUNNING
		// transition, so cancellation is final and releases its reservation.
		j.Status, j.FinishedAt = model.JobCanceled, &now
		clearJobAssignment(j)
	case model.JobRunning:
		j.Status = model.JobCanceling
	case model.JobCanceling:
		// Explicit user cancellation wins over an automatic recovery retry or
		// exhausted-recovery disposition, while retaining the cleanup fence.
		if j.Error == recoveryRetryMarker || j.Error == recoveryCleanupMarker {
			j.Error = ""
		}
	case model.JobCanceled:
		// Idempotent.
	default:
		return nil, errors.New("completed job cannot be canceled")
	}
	j.UpdatedAt = now
	if nodeID != "" {
		s.refreshNodeUsageLocked(nodeID)
	}
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
	copy.AllocatedGPUs = append([]int(nil), j.AllocatedGPUs...)
	copy.LeaseExpiresAt = cloneTime(j.LeaseExpiresAt)
	return &copy, nil
}

func (s *Store) ListJobs() []*model.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*model.Job, 0, len(s.state.Jobs))
	for _, j := range s.state.Jobs {
		copy := *j
		copy.AllocatedGPUs = append([]int(nil), j.AllocatedGPUs...)
		copy.LeaseExpiresAt = cloneTime(j.LeaseExpiresAt)
		result = append(result, &copy)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

// RegisterNode is retained for direct store callers. HTTP agents use
// RegisterNodeSession so concurrent processes with the same node ID cannot
// take over one another while the current session is alive.
func (s *Store) RegisterNode(in model.Node) (*model.Node, error) {
	return s.registerNode(in, newToken("session"), true, false)
}

func (s *Store) RegisterNodeSession(in model.Node, session string) (*model.Node, error) {
	return s.registerNode(in, strings.TrimSpace(session), false, true)
}

func (s *Store) registerNode(in model.Node, session string, forceTakeover, requireCleanup bool) (*model.Node, error) {
	if session == "" {
		return nil, ErrAgentSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if in.HealthStatus == "" {
		in.HealthStatus = "HEALTHY"
	}
	if s.nodeHealthEnabled {
		status := strings.ToUpper(strings.TrimSpace(in.HealthStatus))
		if status != "HEALTHY" && status != "DEGRADED" {
			return nil, errors.New("health status must be HEALTHY or DEGRADED")
		}
		in.HealthStatus, in.LastHealthCheck = status, &now
	}
	existing := s.state.Nodes[in.ID]
	if existing != nil && existing.SessionEpoch != "" && existing.SessionEpoch != session && !forceTakeover && now.Sub(existing.LastHeartbeat) <= agentSessionActiveFor {
		return nil, ErrAgentSessionActive
	}
	if err := s.validateAndAdmitNodeLocked(in, existing); err != nil {
		return nil, err
	}
	before := cloneSnapshot(s.state)
	if existing != nil && existing.SessionEpoch != session {
		s.recoverNodeJobsLocked(in.ID, session, now)
	}
	in.SessionEpoch = session
	in.CleanupPending = requireCleanup
	in.LastHeartbeat = now
	s.state.Nodes[in.ID] = &in
	s.refreshNodeUsageLocked(in.ID)
	if err := s.commitLocked(before); err != nil {
		return nil, err
	}
	copy := in
	copy.Devices = append([]model.GPUDevice(nil), in.Devices...)
	copy.LastHealthCheck = cloneTime(in.LastHealthCheck)
	return &copy, nil
}

// ConfirmNodeCleanupSession opens a freshly registered session for dispatch
// only after that exact Agent session has removed all locally managed legacy
// containers. The pending bit is persisted with the node so a control-plane
// restart cannot accidentally bypass the takeover gate.
func (s *Store) ConfirmNodeCleanupSession(id, session string) (*model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.state.Nodes[id]
	if node == nil {
		return nil, ErrNotFound
	}
	if err := s.validateAgentSessionLocked(node, session, time.Now().UTC(), false, true); err != nil {
		return nil, err
	}
	if !node.CleanupPending {
		return nil, ErrNodeUnavailable
	}
	before := cloneSnapshot(s.state)
	node.CleanupPending = false
	node.LastHeartbeat = time.Now().UTC()
	if err := s.commitLocked(before); err != nil {
		return nil, err
	}
	copy := *node
	copy.ActiveJobs = append([]string(nil), node.ActiveJobs...)
	copy.Devices = append([]model.GPUDevice(nil), node.Devices...)
	copy.LastHealthCheck = cloneTime(node.LastHealthCheck)
	return &copy, nil
}

// validateAgentSessionLocked is the single server-side lease gate for real
// Agent requests. A session that missed its heartbeat deadline can never
// revive itself by making heartbeat (or any other Agent) request first. The
// fence is persisted before the request is rejected so a control-plane restart
// cannot restore the expired owner.
func (s *Store) validateAgentSessionLocked(node *model.Node, session string, now time.Time, requireReady, enforceLease bool) error {
	if enforceLease && node.SessionEpoch != "" && now.Sub(node.LastHeartbeat) > agentSessionActiveFor {
		before := cloneSnapshot(s.state)
		node.SessionEpoch = ""
		node.CleanupPending = true
		if err := s.commitLocked(before); err != nil {
			return err
		}
		return ErrAgentSession
	}
	if session = strings.TrimSpace(session); session == "" || node.SessionEpoch != session {
		return ErrAgentSession
	}
	if requireReady && node.CleanupPending {
		return ErrNodeUnavailable
	}
	return nil
}

func (s *Store) validateAndAdmitNodeLocked(candidate model.Node, existing *model.Node) error {
	if err := validateNodeResources(candidate, s.perGPUInventoryEnabled && strings.EqualFold(candidate.HealthStatus, "HEALTHY")); err != nil {
		return err
	}
	projectedNodes := len(s.state.Nodes)
	projectedGPUs := 0
	for id, node := range s.state.Nodes {
		if existing != nil && id == existing.ID {
			continue
		}
		count := node.GPUCount
		if count < 0 {
			count = 0
		}
		if count > math.MaxInt-projectedGPUs {
			return fmt.Errorf("%w: GPU total overflows", ErrInvalidResources)
		}
		projectedGPUs += count
	}
	if existing == nil {
		if projectedNodes == math.MaxInt {
			return fmt.Errorf("%w: node total overflows", ErrInvalidResources)
		}
		projectedNodes++
	}
	if candidate.GPUCount > math.MaxInt-projectedGPUs {
		return fmt.Errorf("%w: GPU total overflows", ErrInvalidResources)
	}
	projectedGPUs += candidate.GPUCount
	increasesCapacity := existing == nil || candidate.GPUCount > existing.GPUCount
	if increasesCapacity && !s.licenseExpiresAt.IsZero() && !time.Now().UTC().Before(s.licenseExpiresAt) {
		return ErrLicenseExpired
	}
	if existing == nil && s.maxNodes > 0 && projectedNodes > s.maxNodes {
		return fmt.Errorf("%w: %d nodes requested, %d licensed", ErrLicenseCapacity, projectedNodes, s.maxNodes)
	}
	if increasesCapacity && s.maxGPUs > 0 && projectedGPUs > s.maxGPUs {
		return fmt.Errorf("%w: %d GPUs requested, %d licensed", ErrLicenseCapacity, projectedGPUs, s.maxGPUs)
	}
	return nil
}

func validateNodeResources(node model.Node, requireInventory bool) error {
	if node.GPUCount < 0 || node.CPUCores < 0 || node.VRAMGB < 0 {
		return fmt.Errorf("%w: node resource counts cannot be negative", ErrInvalidResources)
	}
	if node.GPUCount > maxGPUsPerNodeOrJob {
		return fmt.Errorf("%w: node gpu_count cannot exceed %d", ErrInvalidResources, maxGPUsPerNodeOrJob)
	}
	if requireInventory && len(node.Devices) != node.GPUCount {
		return fmt.Errorf("%w: gpu_count=%d does not match %d devices", ErrInvalidResources, node.GPUCount, len(node.Devices))
	}
	if len(node.Devices) == 0 {
		return nil
	}
	if len(node.Devices) != node.GPUCount {
		return fmt.Errorf("%w: gpu_count=%d does not match %d devices", ErrInvalidResources, node.GPUCount, len(node.Devices))
	}
	indices, uuids := map[int]bool{}, map[string]bool{}
	minimumVRAM := math.MaxInt
	modelName := ""
	for _, device := range node.Devices {
		if device.Index < 0 || device.Index >= node.GPUCount || indices[device.Index] {
			return fmt.Errorf("%w: GPU device indexes must be unique and within gpu_count", ErrInvalidResources)
		}
		indices[device.Index] = true
		uuid := strings.TrimSpace(device.UUID)
		if requireInventory && uuid == "" {
			return fmt.Errorf("%w: GPU device UUID is required", ErrInvalidResources)
		}
		if uuid != "" && uuids[uuid] {
			return fmt.Errorf("%w: GPU device UUIDs must be unique", ErrInvalidResources)
		}
		uuids[uuid] = uuid != ""
		if device.VRAMGB <= 0 || strings.TrimSpace(device.Model) == "" {
			return fmt.Errorf("%w: GPU model and positive VRAM are required", ErrInvalidResources)
		}
		if device.VRAMGB < minimumVRAM {
			minimumVRAM = device.VRAMGB
		}
		if modelName == "" {
			modelName = device.Model
		} else if modelName != device.Model {
			modelName = "mixed"
		}
	}
	if node.VRAMGB != minimumVRAM || node.GPUModel != modelName {
		return fmt.Errorf("%w: aggregate GPU model or VRAM does not match devices", ErrInvalidResources)
	}
	return nil
}

func (s *Store) recoverNodeJobsLocked(nodeID, session string, now time.Time) {
	for _, job := range s.state.Jobs {
		if job.AssignedNode != nodeID || !activeJob(job.Status) {
			continue
		}
		job.AssignedSession, job.AttemptToken, job.LeaseExpiresAt = session, "", nil
		if job.Status != model.JobRunning {
			continue
		}
		// Every interrupted RUNNING attempt first becomes a cleanup-only
		// dispatch and keeps its allocation. Only its cleanup acknowledgement
		// may requeue or fail it, so health/license changes cannot release a GPU
		// while the previous container may still exist.
		job.Status, job.FinishedAt = model.JobCanceling, nil
		if job.Attempts > job.MaxRetries {
			job.Error, job.UpdatedAt = recoveryCleanupMarker, now
		} else {
			job.Recoveries++
			job.Error, job.UpdatedAt = recoveryRetryMarker, now
		}
	}
}

func (s *Store) HeartbeatNode(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.state.Nodes[id]
	if node == nil {
		return ErrNotFound
	}
	return s.heartbeatNodeLocked(node, node.SessionEpoch, true)
}

func (s *Store) HeartbeatNodeSession(id, session string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.state.Nodes[id]
	if node == nil {
		return ErrNotFound
	}
	return s.heartbeatNodeLocked(node, session, false)
}

func (s *Store) heartbeatNodeLocked(node *model.Node, session string, trusted bool) error {
	if err := s.validateAgentSessionLocked(node, session, time.Now().UTC(), !trusted, !trusted); err != nil {
		return err
	}
	before := cloneSnapshot(s.state)
	node.LastHeartbeat = time.Now().UTC()
	return s.commitLocked(before)
}

func (s *Store) UpdateNodeHealth(id string, update model.NodeHealthUpdate) (*model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.state.Nodes[id]
	if node == nil {
		return nil, ErrNotFound
	}
	return s.updateNodeHealthLocked(node, node.SessionEpoch, update, true)
}

func (s *Store) UpdateNodeHealthSession(id, session string, update model.NodeHealthUpdate) (*model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.state.Nodes[id]
	if node == nil {
		return nil, ErrNotFound
	}
	return s.updateNodeHealthLocked(node, session, update, false)
}

func (s *Store) updateNodeHealthLocked(node *model.Node, session string, update model.NodeHealthUpdate, trusted bool) (*model.Node, error) {
	if err := s.validateAgentSessionLocked(node, session, time.Now().UTC(), !trusted, !trusted); err != nil {
		return nil, err
	}
	status := strings.ToUpper(strings.TrimSpace(update.Status))
	if status != "HEALTHY" && status != "DEGRADED" {
		return nil, errors.New("health status must be HEALTHY or DEGRADED")
	}
	if update.GPUCount < 0 || update.VRAMGB < 0 {
		return nil, fmt.Errorf("%w: node resource counts cannot be negative", ErrInvalidResources)
	}
	inventoryChanged := status == "HEALTHY" && !sameInventory(node, update)
	if status == "HEALTHY" {
		candidate := *node
		candidate.Devices = append([]model.GPUDevice(nil), update.Devices...)
		candidate.GPUModel, candidate.GPUCount, candidate.VRAMGB = update.GPUModel, update.GPUCount, update.VRAMGB
		candidate.DriverVersion, candidate.DockerVersion = update.DriverVersion, update.DockerVersion
		candidate.HealthStatus = status
		if err := s.validateAndAdmitNodeLocked(candidate, node); err != nil {
			return nil, err
		}
	}
	before := cloneSnapshot(s.state)
	now := time.Now().UTC()
	node.HealthStatus, node.HealthReason, node.LastHealthCheck = status, strings.TrimSpace(update.Reason), &now
	if status == "HEALTHY" {
		node.Devices = append([]model.GPUDevice(nil), update.Devices...)
		node.GPUModel, node.GPUCount, node.VRAMGB = update.GPUModel, update.GPUCount, update.VRAMGB
		node.DriverVersion, node.DockerVersion = update.DriverVersion, update.DockerVersion
	}
	if status == "DEGRADED" || inventoryChanged {
		s.requeueAssignedJobsLocked(node.ID, now)
	}
	s.refreshNodeUsageLocked(node.ID)
	if err := s.commitLocked(before); err != nil {
		return nil, err
	}
	copy := *node
	copy.Devices = append([]model.GPUDevice(nil), node.Devices...)
	copy.LastHealthCheck = cloneTime(node.LastHealthCheck)
	return &copy, nil
}

func sameInventory(node *model.Node, update model.NodeHealthUpdate) bool {
	if node.GPUModel != update.GPUModel || node.GPUCount != update.GPUCount || node.VRAMGB != update.VRAMGB || len(node.Devices) != len(update.Devices) {
		return false
	}
	for index := range node.Devices {
		if node.Devices[index] != update.Devices[index] {
			return false
		}
	}
	return true
}

func clearJobAssignment(job *model.Job) {
	job.AssignedNode, job.AssignedSession, job.AttemptToken = "", "", ""
	job.AllocatedGPUs, job.LeaseExpiresAt = nil, nil
}

func (s *Store) requeueAssignedJobsLocked(nodeID string, now time.Time) {
	for _, job := range s.state.Jobs {
		if job.AssignedNode != nodeID || job.Status != model.JobAssigned {
			continue
		}
		job.Status, job.UpdatedAt = model.JobQueued, now
		if job.Attempts > 0 {
			job.Attempts--
		}
		clearJobAssignment(job)
	}
}

// reconcileOfflineJobsLocked fences sessions whose owning Agent has remained
// offline past the takeover boundary. Active assignments intentionally retain
// their status and capacity: without a cleanup confirmation from the physical
// node, the control plane cannot prove that an orphaned container stopped and
// must not retry the same workload elsewhere.
func (s *Store) reconcileOfflineJobsLocked(now time.Time, offlineAfter time.Duration) bool {
	changed := false
	for _, job := range s.state.Jobs {
		if !activeJob(job.Status) {
			continue
		}
		node := s.state.Nodes[job.AssignedNode]
		if node == nil || now.Sub(node.LastHeartbeat) <= offlineAfter {
			continue
		}
		if node.SessionEpoch != "" || !node.CleanupPending {
			node.SessionEpoch = ""
			node.CleanupPending = true
			changed = true
		}
	}
	return changed
}

func (s *Store) ListNodes() []*model.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	result := make([]*model.Node, 0, len(s.state.Nodes))
	for _, n := range s.state.Nodes {
		copy := *n
		copy.ActiveJobs = append([]string(nil), n.ActiveJobs...)
		copy.Devices = append([]model.GPUDevice(nil), n.Devices...)
		copy.LastHealthCheck = cloneTime(n.LastHealthCheck)
		if s.nodeHealthEnabled && !s.nodeHealthyLocked(n, now) && !strings.EqualFold(n.HealthStatus, "DEGRADED") {
			copy.HealthStatus, copy.HealthReason = "DEGRADED", "health report stale"
		}
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

func activeJob(status model.JobStatus) bool {
	return status == model.JobAssigned || status == model.JobRunning || status == model.JobCanceling
}

func (s *Store) usedGPUSetLocked(nodeID string) map[int]bool {
	used := map[int]bool{}
	nodeGPUCount := 0
	if node := s.state.Nodes[nodeID]; node != nil && node.GPUCount > 0 {
		nodeGPUCount = node.GPUCount
		if nodeGPUCount > maxGPUsPerNodeOrJob {
			nodeGPUCount = maxGPUsPerNodeOrJob
		}
	}
	for _, job := range s.state.Jobs {
		if job.AssignedNode != nodeID || !activeJob(job.Status) {
			continue
		}
		allocated := job.AllocatedGPUs
		if len(allocated) == 0 && s.gpuGranularScheduling {
			count := job.Requirements.GPUCount
			if count < 0 {
				count = 0
			}
			if count > nodeGPUCount {
				count = nodeGPUCount
			}
			for index := 0; index < count; index++ {
				allocated = append(allocated, index)
			}
		}
		for _, index := range allocated {
			used[index] = true
		}
	}
	return used
}

func (s *Store) availableGPUsLocked(node *model.Node, count int) []int {
	if count <= 0 || node.GPUCount <= 0 {
		return nil
	}
	capacity := count
	if capacity > node.GPUCount {
		capacity = node.GPUCount
	}
	if capacity > maxGPUsPerNodeOrJob {
		capacity = maxGPUsPerNodeOrJob
	}
	used := s.usedGPUSetLocked(node.ID)
	available := make([]int, 0, capacity)
	for index := 0; index < node.GPUCount && len(available) < capacity; index++ {
		if !used[index] {
			available = append(available, index)
		}
	}
	return available
}

func (s *Store) hasActiveCPUOnlyJobLocked(nodeID string) bool {
	for _, job := range s.state.Jobs {
		if job.AssignedNode == nodeID && activeJob(job.Status) && job.Requirements.GPUCount == 0 {
			return true
		}
	}
	return false
}

func (s *Store) refreshNodeUsageLocked(nodeID string) {
	node := s.state.Nodes[nodeID]
	if node == nil {
		return
	}
	node.ActiveJobs = nil
	node.AllocatedGPUs = 0
	for _, job := range s.state.Jobs {
		if job.AssignedNode == nodeID && activeJob(job.Status) {
			node.ActiveJobs = append(node.ActiveJobs, job.ID)
			if s.gpuGranularScheduling {
				node.AllocatedGPUs += len(job.AllocatedGPUs)
			} else {
				node.AllocatedGPUs = node.GPUCount
			}
		}
	}
	sort.Strings(node.ActiveJobs)
	node.Busy = len(node.ActiveJobs) > 0
	node.CurrentJob = ""
	if len(node.ActiveJobs) > 0 {
		node.CurrentJob = node.ActiveJobs[0]
	}
}

func (s *Store) nodeHealthyLocked(node *model.Node, now time.Time) bool {
	if !s.nodeHealthEnabled {
		return !strings.EqualFold(node.HealthStatus, "DEGRADED")
	}
	if !strings.EqualFold(node.HealthStatus, "HEALTHY") || node.LastHealthCheck == nil {
		return false
	}
	ttl := s.nodeHealthTTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return !now.After(node.LastHealthCheck.Add(ttl))
}

func (s *Store) eligibleLocked(j *model.Job, n *model.Node, now time.Time, offlineAfter time.Duration) bool {
	if n.CleanupPending || n.SessionEpoch == "" {
		return false
	}
	if now.Sub(n.LastHeartbeat) > offlineAfter {
		return false
	}
	if !s.nodeHealthyLocked(n, now) {
		return false
	}
	r := j.Requirements
	if r.GPUCount > 0 {
		if n.GPUCount < r.GPUCount || n.VRAMGB < r.MinVRAMGB || !containsFold(r.GPUModels, n.GPUModel) {
			return false
		}
	}
	if r.MaxHourly > 0 && n.HourlyPrice > r.MaxHourly {
		return false
	}
	if !containsFold(r.Providers, n.Provider) || !containsFold(r.Pools, n.Pool) {
		return false
	}
	for k, v := range r.Labels {
		if n.Labels[k] != v {
			return false
		}
	}
	return true
}

func (s *Store) licensedNodeSetLocked(now time.Time) map[string]bool {
	allowed := map[string]bool{}
	if !s.licenseExpiresAt.IsZero() && !now.Before(s.licenseExpiresAt) {
		return allowed
	}
	nodes := make([]*model.Node, 0, len(s.state.Nodes))
	for _, node := range s.state.Nodes {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	usedGPUs := 0
	for _, node := range nodes {
		if validateNodeResources(*node, s.perGPUInventoryEnabled && strings.EqualFold(node.HealthStatus, "HEALTHY")) != nil {
			continue
		}
		if s.maxNodes > 0 && len(allowed) >= s.maxNodes {
			continue
		}
		if node.GPUCount > math.MaxInt-usedGPUs || (s.maxGPUs > 0 && usedGPUs+node.GPUCount > s.maxGPUs) {
			continue
		}
		allowed[node.ID] = true
		usedGPUs += node.GPUCount
	}
	return allowed
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
	before := cloneSnapshot(s.state)
	changed := s.reconcileOfflineJobsLocked(now, offlineAfter)
	licensedNodes := s.licensedNodeSetLocked(now)
	jobs := make([]*model.Job, 0)
	for _, j := range s.state.Jobs {
		if j.Status == model.JobQueued {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	for _, j := range jobs {
		var best *model.Node
		for _, n := range s.state.Nodes {
			if _, licensed := licensedNodes[n.ID]; !licensed {
				continue
			}
			capacityAvailable := !n.Busy
			if s.gpuGranularScheduling {
				if j.Requirements.GPUCount == 0 {
					// CPU-only jobs remain whole-node exclusive until CPU accounting is implemented.
					capacityAvailable = !n.Busy
				} else if j.Requirements.GPUCount > n.GPUCount {
					capacityAvailable = false
				} else {
					capacityAvailable = !s.hasActiveCPUOnlyJobLocked(n.ID) && len(s.availableGPUsLocked(n, j.Requirements.GPUCount)) == j.Requirements.GPUCount
				}
			}
			if capacityAvailable && s.eligibleLocked(j, n, now, offlineAfter) && betterNode(j.Strategy, n, best) {
				best = n
			}
		}
		if best == nil {
			continue
		}
		if s.gpuGranularScheduling {
			j.AllocatedGPUs = s.availableGPUsLocked(best, j.Requirements.GPUCount)
		} else {
			j.AllocatedGPUs = nil
		}
		j.Status, j.AssignedNode, j.AssignedSession, j.UpdatedAt = model.JobAssigned, best.ID, best.SessionEpoch, now
		j.AttemptToken, j.LeaseExpiresAt = "", nil
		j.Attempts++
		s.refreshNodeUsageLocked(best.ID)
		changed = true
	}
	if changed {
		if err := s.commitLocked(before); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) NextJob(nodeID string) (*model.AgentJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.state.Nodes[nodeID]
	if node == nil {
		return nil, ErrNotFound
	}
	return s.nextJobLocked(node, node.SessionEpoch, true)
}

func (s *Store) NextJobSession(nodeID, session string) (*model.AgentJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.state.Nodes[nodeID]
	if node == nil {
		return nil, ErrNotFound
	}
	return s.nextJobLocked(node, session, false)
}

func (s *Store) nextJobLocked(node *model.Node, session string, trusted bool) (*model.AgentJob, error) {
	if err := s.validateAgentSessionLocked(node, session, time.Now().UTC(), !trusted, !trusted); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var selected *model.Job
	for _, candidate := range s.state.Jobs {
		if candidate.AssignedNode != node.ID || candidate.AssignedSession != session || candidate.Status != model.JobCanceling {
			continue
		}
		if candidate.AttemptToken != "" && (candidate.LeaseExpiresAt == nil || now.Before(*candidate.LeaseExpiresAt)) {
			continue
		}
		if selected == nil || candidate.CreatedAt.Before(selected.CreatedAt) {
			selected = candidate
		}
	}
	_, licensed := s.licensedNodeSetLocked(now)[node.ID]
	if selected == nil && (!licensed || !s.nodeHealthyLocked(node, now)) {
		before := cloneSnapshot(s.state)
		s.requeueAssignedJobsLocked(node.ID, now)
		s.refreshNodeUsageLocked(node.ID)
		if err := s.commitLocked(before); err != nil {
			return nil, err
		}
		return nil, ErrNodeUnavailable
	}
	if selected == nil {
		for _, candidate := range s.state.Jobs {
			if candidate.AssignedNode != node.ID || candidate.AssignedSession != session || candidate.Status != model.JobAssigned {
				continue
			}
			if candidate.AttemptToken != "" {
				// Running attempts keep an unbounded lease until their owner
				// confirms completion. Only short pre-start claims expire.
				if candidate.LeaseExpiresAt == nil || now.Before(*candidate.LeaseExpiresAt) {
					continue
				}
			}
			if selected == nil || candidate.CreatedAt.Before(selected.CreatedAt) {
				selected = candidate
			}
		}
	}
	if selected == nil {
		return nil, nil
	}
	before := cloneSnapshot(s.state)
	leaseExpiresAt := now.Add(jobLeaseDuration)
	selected.AttemptToken, selected.LeaseExpiresAt = newToken("attempt"), &leaseExpiresAt
	selected.UpdatedAt = now
	if err := s.commitLocked(before); err != nil {
		return nil, err
	}
	copy := *selected
	copy.AllocatedGPUs = append([]int(nil), selected.AllocatedGPUs...)
	copy.LeaseExpiresAt = cloneTime(selected.LeaseExpiresAt)
	return &model.AgentJob{Job: copy, AttemptToken: selected.AttemptToken}, nil
}

func (s *Store) UpdateJob(id, nodeID string, in model.JobUpdate) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.state.Jobs[id]
	if job == nil {
		return nil, ErrNotFound
	}
	return s.updateJobLocked(job, nodeID, job.AssignedSession, job.AttemptToken, in, true)
}

func (s *Store) UpdateJobLease(id, nodeID, session, attemptToken string, in model.JobUpdate) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.state.Jobs[id]
	if job == nil {
		return nil, ErrNotFound
	}
	return s.updateJobLocked(job, nodeID, strings.TrimSpace(session), strings.TrimSpace(attemptToken), in, false)
}

func (s *Store) updateJobLocked(job *model.Job, nodeID, session, attemptToken string, in model.JobUpdate, trusted bool) (*model.Job, error) {
	if job.AssignedNode != nodeID {
		return nil, errors.New("job is not assigned to this node")
	}
	node := s.state.Nodes[nodeID]
	if node == nil {
		return nil, ErrNotFound
	}
	if !trusted {
		if err := s.validateAgentSessionLocked(node, session, time.Now().UTC(), true, true); err != nil {
			return nil, err
		}
		if attemptToken == "" || job.AssignedSession != strings.TrimSpace(session) || job.AttemptToken != strings.TrimSpace(attemptToken) {
			return nil, ErrAttemptLease
		}
	}
	now := time.Now().UTC()
	before := cloneSnapshot(s.state)
	resultError := in.Error
	resultOutput := in.Output
	switch in.Status {
	case model.JobRunning:
		if job.Status != model.JobAssigned {
			return nil, errors.New("job is not assigned")
		}
		_, licensed := s.licensedNodeSetLocked(now)[nodeID]
		if (!trusted && node.CleanupPending) || !licensed || !s.nodeHealthyLocked(node, now) {
			s.requeueAssignedJobsLocked(nodeID, now)
			s.refreshNodeUsageLocked(nodeID)
			if err := s.commitLocked(before); err != nil {
				return nil, err
			}
			return nil, ErrNodeUnavailable
		}
		job.Status, job.StartedAt, job.LeaseExpiresAt = model.JobRunning, &now, nil
	case model.JobSucceeded:
		if job.Status != model.JobRunning {
			return nil, errors.New("job is not running")
		}
		job.Status, job.FinishedAt = model.JobSucceeded, &now
		job.AttemptToken, job.LeaseExpiresAt, job.AssignedSession = "", nil, ""
	case model.JobFailed:
		if job.Status != model.JobRunning {
			return nil, errors.New("job is not running")
		}
		job.FinishedAt = &now
		if job.Attempts <= job.MaxRetries {
			job.Status = model.JobQueued
			clearJobAssignment(job)
			job.StartedAt, job.FinishedAt = nil, nil
		} else {
			job.Status = model.JobFailed
			job.AttemptToken, job.LeaseExpiresAt, job.AssignedSession = "", nil, ""
		}
	case model.JobCanceled:
		if job.Status != model.JobCanceling {
			return nil, errors.New("job is not canceling")
		}
		if job.Error == recoveryRetryMarker {
			job.Status = model.JobQueued
			clearJobAssignment(job)
			job.StartedAt, job.FinishedAt = nil, nil
			resultOutput, resultError = "", ""
		} else if job.Error == recoveryCleanupMarker {
			job.Status = model.JobFailed
			resultError = "agent restarted after interruption; retry budget exhausted"
		} else {
			job.Status = model.JobCanceled
		}
		if job.Status != model.JobQueued {
			job.FinishedAt = &now
			job.AttemptToken, job.LeaseExpiresAt, job.AssignedSession = "", nil, ""
		}
	default:
		return nil, errors.New("unsupported status transition")
	}
	job.Output, job.Error, job.UpdatedAt = resultOutput, resultError, now
	s.refreshNodeUsageLocked(nodeID)
	if err := s.commitLocked(before); err != nil {
		return nil, err
	}
	copy := *job
	copy.AllocatedGPUs = append([]int(nil), job.AllocatedGPUs...)
	return &copy, nil
}

func (s *Store) UpdateJobOutput(id, nodeID, output string) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.state.Jobs[id]
	if job == nil {
		return nil, ErrNotFound
	}
	return s.updateJobOutputLocked(job, nodeID, job.AssignedSession, job.AttemptToken, output, true)
}

func (s *Store) UpdateJobOutputLease(id, nodeID, session, attemptToken, output string) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.state.Jobs[id]
	if job == nil {
		return nil, ErrNotFound
	}
	return s.updateJobOutputLocked(job, nodeID, strings.TrimSpace(session), strings.TrimSpace(attemptToken), output, false)
}

func (s *Store) updateJobOutputLocked(job *model.Job, nodeID, session, attemptToken, output string, trusted bool) (*model.Job, error) {
	if job.AssignedNode != nodeID {
		return nil, errors.New("job is not assigned to this node")
	}
	node := s.state.Nodes[nodeID]
	if !trusted {
		if node == nil {
			return nil, ErrNotFound
		}
		if err := s.validateAgentSessionLocked(node, session, time.Now().UTC(), true, true); err != nil {
			return nil, err
		}
		if attemptToken == "" || job.AssignedSession != strings.TrimSpace(session) || job.AttemptToken != strings.TrimSpace(attemptToken) {
			return nil, ErrAttemptLease
		}
	}
	if job.Status != model.JobRunning && job.Status != model.JobCanceling {
		return nil, errors.New("job is not running")
	}
	if len(output) > 64<<10 {
		output = output[len(output)-(64<<10):]
	}
	before := cloneSnapshot(s.state)
	job.Output = output
	job.UpdatedAt = time.Now().UTC()
	if err := s.commitLocked(before); err != nil {
		return nil, err
	}
	copy := *job
	return &copy, nil
}

func (s *Store) ValidateJobAttempt(id, nodeID, session, attemptToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.validateJobAttemptLocked(id, nodeID, session, attemptToken)
}

// CommitJobAttempt runs commit only while the Agent session and attempt lease
// are still valid. Holding the Store lock across the final, atomic object-store
// promotion linearizes it with node takeover and cleanup fencing without
// holding the lock during the potentially large staging upload.
func (s *Store) CommitJobAttempt(id, nodeID, session, attemptToken string, commit func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateJobAttemptLocked(id, nodeID, session, attemptToken); err != nil {
		return err
	}
	if commit == nil {
		return errors.New("artifact commit callback is required")
	}
	return commit()
}

func (s *Store) validateJobAttemptLocked(id, nodeID, session, attemptToken string) error {
	job := s.state.Jobs[id]
	if job == nil {
		return ErrNotFound
	}
	node := s.state.Nodes[nodeID]
	if node == nil {
		return ErrNotFound
	}
	if err := s.validateAgentSessionLocked(node, session, time.Now().UTC(), true, true); err != nil {
		return err
	}
	session, attemptToken = strings.TrimSpace(session), strings.TrimSpace(attemptToken)
	if attemptToken == "" || job.AssignedNode != nodeID || job.AssignedSession != session || job.AttemptToken != attemptToken {
		return ErrAttemptLease
	}
	if job.Status != model.JobRunning && job.Status != model.JobCanceling {
		return ErrAttemptLease
	}
	return nil
}
