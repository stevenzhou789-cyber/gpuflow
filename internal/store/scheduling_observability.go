package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gpuflow/internal/model"
	"gpuflow/pkg/projectscope"
)

const (
	SchedulingAlgorithmFIFO         = "fifo"
	SchedulingAlgorithmWeightedFair = "weighted_fair"

	SchedulingReasonFIFOSelected          = "fifo_selected"
	SchedulingReasonWeightedFairSelected  = "weighted_fair_selected"
	SchedulingReasonAssigned              = "assigned"
	SchedulingReasonNotQueued             = "not_queued"
	SchedulingReasonProjectDisabled       = "project_disabled"
	SchedulingReasonNoRegisteredNodes     = "no_registered_nodes"
	SchedulingReasonRequirementsUnmatched = "requirements_unmatched"
	SchedulingReasonLicenseUnavailable    = "license_capacity_unavailable"
	SchedulingReasonNodesNotReady         = "nodes_not_ready"
	SchedulingReasonNodesInMaintenance    = "nodes_in_maintenance"
	SchedulingReasonInsufficientCapacity  = "insufficient_free_capacity"
	SchedulingReasonProjectQueueOrder     = "project_queue_order"
	SchedulingReasonFairShareWait         = "fair_share_wait"
	SchedulingReasonAwaitingDispatch      = "awaiting_dispatch"

	defaultSchedulingDecisionLimit = 50
	maxSchedulingDecisionLimit     = 100
)

type SchedulingDecisionQuery struct {
	ProjectID string
	JobID     string
	Limit     int
}

type SchedulingDecisionPage struct {
	Items []*model.SchedulingDecision `json:"items"`
	Total int                         `json:"total"`
	Limit int                         `json:"limit"`
}

func normalizeSchedulingDecisionQuery(query SchedulingDecisionQuery) SchedulingDecisionQuery {
	query.ProjectID = strings.TrimSpace(query.ProjectID)
	if query.ProjectID != "" {
		query.ProjectID = normalizeProjectID(query.ProjectID)
	}
	query.JobID = strings.TrimSpace(query.JobID)
	if query.Limit <= 0 {
		query.Limit = defaultSchedulingDecisionLimit
	}
	if query.Limit > maxSchedulingDecisionLimit {
		query.Limit = maxSchedulingDecisionLimit
	}
	return query
}

func cloneSchedulingDecision(source model.SchedulingDecision) model.SchedulingDecision {
	copy := source
	copy.AllocatedGPUs = append([]int(nil), source.AllocatedGPUs...)
	return copy
}

func schedulingGPUCost(job *model.Job) int {
	cost := job.Requirements.GPUCount
	if cost < 1 {
		return 1
	}
	return cost
}

func (s *Store) newSchedulingDecisionLocked(job *model.Job, project *model.Project, node *model.Node, decidedAt time.Time, algorithm, reasonCode string, vruntimeBefore, vruntimeAfter, strideDelta int64, eligibleNodeCount, runnableProjectCount int) model.SchedulingDecision {
	weight := DefaultProjectWeight
	if project != nil && project.Weight >= MinProjectWeight {
		weight = project.Weight
	}
	vendor, runtimeName := "", ""
	if node.GPUCount > 0 {
		vendor, runtimeName = acceleratorIdentity(node.Labels)
	}
	return model.SchedulingDecision{
		DecisionID: newID("decision"), DecidedAt: decidedAt, JobID: job.ID, JobCreatedAt: job.CreatedAt,
		ProjectID: projectIDOf(job), Attempt: job.Attempts, NodeID: node.ID,
		AllocatedGPUs: append([]int(nil), job.AllocatedGPUs...), Algorithm: algorithm, ReasonCode: reasonCode,
		Priority: job.Priority, GPUCost: schedulingGPUCost(job), ProjectWeight: weight,
		ProjectVRuntimeBefore: vruntimeBefore, ProjectVRuntimeAfter: vruntimeAfter, StrideDelta: strideDelta,
		Strategy: job.Strategy, NodeVendor: vendor, NodeRuntime: runtimeName, NodeModel: node.GPUModel,
		NodeVRAMGB: node.VRAMGB, NodeHourlyPrice: node.HourlyPrice,
		EligibleNodeCount: eligibleNodeCount, RunnableProjectCount: runnableProjectCount,
	}
}

func (s *Store) ListSchedulingDecisions(query SchedulingDecisionQuery) (SchedulingDecisionPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listSchedulingDecisionsLocked(normalizeSchedulingDecisionQuery(query))
}

func (s *Store) listSchedulingDecisionsLocked(query SchedulingDecisionQuery) (SchedulingDecisionPage, error) {
	if s.db != nil {
		return s.listMySQLSchedulingDecisionsLocked(query)
	}
	filtered := make([]model.SchedulingDecision, 0, len(s.memorySchedulingDecisions))
	for _, decision := range s.memorySchedulingDecisions {
		if query.ProjectID != "" && decision.ProjectID != query.ProjectID || query.JobID != "" && decision.JobID != query.JobID {
			continue
		}
		filtered = append(filtered, cloneSchedulingDecision(decision))
	}
	sort.Slice(filtered, func(i, j int) bool {
		if !filtered[i].DecidedAt.Equal(filtered[j].DecidedAt) {
			return filtered[i].DecidedAt.After(filtered[j].DecidedAt)
		}
		return filtered[i].DecisionID > filtered[j].DecisionID
	})
	total := len(filtered)
	if len(filtered) > query.Limit {
		filtered = filtered[:query.Limit]
	}
	items := make([]*model.SchedulingDecision, 0, len(filtered))
	for index := range filtered {
		copy := cloneSchedulingDecision(filtered[index])
		items = append(items, &copy)
	}
	return SchedulingDecisionPage{Items: items, Total: total, Limit: query.Limit}, nil
}

func (s *Store) listMySQLSchedulingDecisionsLocked(query SchedulingDecisionQuery) (SchedulingDecisionPage, error) {
	where, args := schedulingDecisionWhere(query)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM scheduling_decisions"+where, args...).Scan(&total); err != nil {
		return SchedulingDecisionPage{}, fmt.Errorf("%w: count scheduling decisions: %v", ErrPersistence, err)
	}
	selectArgs := append(append([]any(nil), args...), query.Limit)
	rows, err := s.db.QueryContext(ctx, `SELECT decision_id, decided_at, job_id, job_created_at, project_id,
  attempt, node_id, allocated_gpus_json, algorithm, reason_code, priority, gpu_cost,
  project_weight, project_vruntime_before, project_vruntime_after, stride_delta, strategy,
  node_vendor, node_runtime, node_model, node_vram_gb, node_hourly_price,
  eligible_node_count, runnable_project_count
FROM scheduling_decisions`+where+" ORDER BY decided_at DESC, decision_id DESC LIMIT ?", selectArgs...)
	if err != nil {
		return SchedulingDecisionPage{}, fmt.Errorf("%w: list scheduling decisions: %v", ErrPersistence, err)
	}
	defer rows.Close()
	items := make([]*model.SchedulingDecision, 0)
	for rows.Next() {
		var decision model.SchedulingDecision
		var allocatedJSON []byte
		if err := rows.Scan(&decision.DecisionID, &decision.DecidedAt, &decision.JobID, &decision.JobCreatedAt,
			&decision.ProjectID, &decision.Attempt, &decision.NodeID, &allocatedJSON, &decision.Algorithm,
			&decision.ReasonCode, &decision.Priority, &decision.GPUCost, &decision.ProjectWeight,
			&decision.ProjectVRuntimeBefore, &decision.ProjectVRuntimeAfter, &decision.StrideDelta,
			&decision.Strategy, &decision.NodeVendor, &decision.NodeRuntime, &decision.NodeModel,
			&decision.NodeVRAMGB, &decision.NodeHourlyPrice, &decision.EligibleNodeCount,
			&decision.RunnableProjectCount); err != nil {
			return SchedulingDecisionPage{}, fmt.Errorf("%w: scan scheduling decision: %v", ErrPersistence, err)
		}
		if err := json.Unmarshal(allocatedJSON, &decision.AllocatedGPUs); err != nil {
			return SchedulingDecisionPage{}, fmt.Errorf("%w: decode scheduling decision GPUs: %v", ErrPersistence, err)
		}
		items = append(items, &decision)
	}
	if err := rows.Err(); err != nil {
		return SchedulingDecisionPage{}, fmt.Errorf("%w: iterate scheduling decisions: %v", ErrPersistence, err)
	}
	return SchedulingDecisionPage{Items: items, Total: total, Limit: query.Limit}, nil
}

func schedulingDecisionWhere(query SchedulingDecisionQuery) (string, []any) {
	clauses := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if query.ProjectID != "" {
		clauses = append(clauses, "project_id = ?")
		args = append(args, query.ProjectID)
	}
	if query.JobID != "" {
		clauses = append(clauses, "job_id = ?")
		args = append(args, query.JobID)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func (s *Store) GetJobSchedulingForScope(scope projectscope.Scope, id string, offlineAfter time.Duration) (*model.JobSchedulingExplanation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.state.Jobs[id]
	if job == nil || !scope.Allows(projectIDOf(job)) {
		return nil, ErrNotFound
	}
	if offlineAfter <= 0 {
		offlineAfter = model.AgentSessionTTL
	}
	return s.explainJobSchedulingLocked(job, time.Now().UTC(), offlineAfter)
}

func (s *Store) explainJobSchedulingLocked(job *model.Job, now time.Time, offlineAfter time.Duration) (*model.JobSchedulingExplanation, error) {
	projectID := projectIDOf(job)
	project := s.state.Projects[projectID]
	explanation := &model.JobSchedulingExplanation{
		JobID: job.ID, ProjectID: projectID, Status: job.Status, EvaluatedAt: now,
		Priority: job.Priority, GPUCost: schedulingGPUCost(job), Quota: s.quotaSnapshotLocked(projectID),
		SchedulerMode: SchedulingAlgorithmFIFO,
	}
	if s.projectFairScheduling {
		explanation.SchedulerMode = SchedulingAlgorithmWeightedFair
	}
	if project != nil {
		explanation.ProjectWeight = project.Weight
		explanation.ProjectVRuntime = project.SchedulerVRuntime
		explanation.MinimumProjectVRuntime = project.SchedulerVRuntime
	}
	if s.projectFairScheduling {
		if minimum, found := s.minimumObservableVRuntimeLocked(now); found {
			explanation.MinimumProjectVRuntime = minimum
			explanation.RelativeProjectVRuntime = explanation.ProjectVRuntime - minimum
		}
	}
	latest, err := s.listSchedulingDecisionsLocked(normalizeSchedulingDecisionQuery(SchedulingDecisionQuery{ProjectID: projectID, JobID: job.ID, Limit: 1}))
	if err != nil {
		return nil, err
	}
	if len(latest.Items) > 0 {
		explanation.LatestDecision = latest.Items[0]
	}
	if job.Status == model.JobAssigned {
		explanation.ReasonCode = SchedulingReasonAssigned
		return explanation, nil
	}
	if job.Status != model.JobQueued {
		explanation.ReasonCode = SchedulingReasonNotQueued
		return explanation, nil
	}
	licensedNodes := s.licensedNodeSetLocked(now)
	explanation.Nodes.Registered = len(s.state.Nodes)
	for _, node := range s.state.Nodes {
		if !s.nodeSatisfiesJobRequirementsLocked(job, node) {
			continue
		}
		explanation.Nodes.RequirementMatched++
		if !licensedNodes[node.ID] {
			continue
		}
		explanation.Nodes.LicensedMatched++
		if node.Maintenance {
			explanation.Nodes.Maintenance++
		}
		if !s.eligibleLocked(job, node, now, offlineAfter) {
			continue
		}
		explanation.Nodes.Ready++
		if s.nodeHasCapacityForJobLocked(job, node) {
			explanation.Nodes.Available++
		}
	}
	if err := s.ensureProjectCanScheduleLocked(job); err != nil {
		var quota *ProjectQuotaError
		switch {
		case errors.As(err, &quota):
			explanation.ReasonCode = quota.Code
		case errors.Is(err, ErrProjectDisabled):
			explanation.ReasonCode = SchedulingReasonProjectDisabled
		default:
			explanation.ReasonCode = SchedulingReasonAwaitingDispatch
		}
		return explanation, nil
	}
	switch {
	case explanation.Nodes.Registered == 0:
		explanation.ReasonCode = SchedulingReasonNoRegisteredNodes
	case explanation.Nodes.RequirementMatched == 0:
		explanation.ReasonCode = SchedulingReasonRequirementsUnmatched
	case explanation.Nodes.LicensedMatched == 0:
		explanation.ReasonCode = SchedulingReasonLicenseUnavailable
	case explanation.Nodes.Maintenance == explanation.Nodes.LicensedMatched:
		explanation.ReasonCode = SchedulingReasonNodesInMaintenance
	case explanation.Nodes.Ready == 0:
		explanation.ReasonCode = SchedulingReasonNodesNotReady
	case explanation.Nodes.Available == 0:
		explanation.ReasonCode = SchedulingReasonInsufficientCapacity
	default:
		explanation.JobsAheadInProject = s.jobsAheadInProjectLocked(job, licensedNodes, now, offlineAfter)
		if explanation.JobsAheadInProject > 0 {
			explanation.ReasonCode = SchedulingReasonProjectQueueOrder
		} else if s.projectFairScheduling {
			selected := s.nextFairCandidateLocked(licensedNodes, now, offlineAfter)
			if selected != nil && selected.job.ID != job.ID {
				explanation.ReasonCode = SchedulingReasonFairShareWait
			} else {
				explanation.ReasonCode = SchedulingReasonAwaitingDispatch
			}
		} else {
			explanation.ReasonCode = SchedulingReasonAwaitingDispatch
		}
	}
	return explanation, nil
}

func (s *Store) jobsAheadInProjectLocked(job *model.Job, licensedNodes map[string]bool, now time.Time, offlineAfter time.Duration) int {
	ahead := 0
	for _, candidate := range s.state.Jobs {
		if candidate.ID == job.ID || candidate.Status != model.JobQueued || projectIDOf(candidate) != projectIDOf(job) {
			continue
		}
		before := candidate.CreatedAt.Before(job.CreatedAt)
		if s.projectFairScheduling {
			before = fairJobBefore(candidate, job)
		}
		if !before {
			continue
		}
		if s.ensureProjectCanScheduleLocked(candidate) != nil || s.bestNodeForJobLocked(candidate, licensedNodes, now, offlineAfter) == nil {
			continue
		}
		ahead++
	}
	return ahead
}

func (s *Store) minimumObservableVRuntimeLocked(now time.Time) (int64, bool) {
	licensedNodes := s.licensedNodeSetLocked(now)
	var minimum int64
	found := false
	for projectID, project := range s.state.Projects {
		if project.Status != model.ProjectActive || !s.projectFairCompetitorLocked(projectID, licensedNodes) {
			continue
		}
		if !found || project.SchedulerVRuntime < minimum {
			minimum = project.SchedulerVRuntime
			found = true
		}
	}
	return minimum, found
}

func (s *Store) ListProjectSchedulingStates() []model.ProjectSchedulingState {
	s.mu.Lock()
	defer s.mu.Unlock()
	minimum := int64(0)
	minimumFound := false
	if s.projectFairScheduling {
		if current, found := s.minimumObservableVRuntimeLocked(time.Now().UTC()); found {
			minimum = current
			minimumFound = true
		}
	}
	result := make([]model.ProjectSchedulingState, 0, len(s.state.Projects))
	for id, project := range s.state.Projects {
		usage := s.quotaSnapshotLocked(id)
		state := model.ProjectSchedulingState{
			ProjectID: id, Status: project.Status, Weight: project.Weight, VRuntime: project.SchedulerVRuntime,
			MinimumVRuntime: project.SchedulerVRuntime, QueuedJobs: usage.QueuedJobs,
			ConcurrentJobs: usage.ConcurrentJobs, AllocatedGPUs: usage.AllocatedGPUs,
		}
		if s.projectFairScheduling && minimumFound {
			state.MinimumVRuntime = minimum
			state.RelativeVRuntime = state.VRuntime - minimum
		}
		result = append(result, state)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ProjectID < result[j].ProjectID })
	return result
}
