package store

import (
	"testing"
	"time"

	"gpuflow/internal/model"
	"gpuflow/pkg/projectscope"
)

func mustSchedulingExplanation(t *testing.T, state *Store, jobID string) *model.JobSchedulingExplanation {
	t.Helper()
	explanation, err := state.GetJobSchedulingForScope(projectscope.All(), jobID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return explanation
}

func TestSchedulingDecisionsAreFeatureGatedImmutableAndRetained(t *testing.T) {
	disabled := NewMemory()
	if _, err := disabled.RegisterNode(model.Node{ID: "disabled-node", GPUCount: 1, VRAMGB: 24}); err != nil {
		t.Fatal(err)
	}
	if _, err := disabled.CreateJob(model.JobCreate{Name: "disabled", Image: "work", Requirements: model.Requirements{GPUCount: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := disabled.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	page, err := disabled.ListSchedulingDecisions(SchedulingDecisionQuery{})
	if err != nil || page.Total != 0 || page.Limit != defaultSchedulingDecisionLimit {
		t.Fatalf("disabled observability recorded decisions: %+v err=%v", page, err)
	}

	state := NewMemory()
	state.SetSchedulingObservability(true)
	state.SetProjectFairScheduling(true)
	state.SetGPUGranularScheduling(true)
	createWeightedProject(t, state, "alpha", 2)
	for _, node := range []model.Node{
		{ID: "cheap", GPUModel: "A100", GPUCount: 1, VRAMGB: 40, HourlyPrice: 1, Labels: map[string]string{model.LabelAcceleratorVendor: model.VendorNVIDIA, model.LabelAcceleratorRuntime: model.RuntimeCUDA}},
		{ID: "expensive", GPUModel: "A100", GPUCount: 1, VRAMGB: 40, HourlyPrice: 2, Labels: map[string]string{model.LabelAcceleratorVendor: model.VendorNVIDIA, model.LabelAcceleratorRuntime: model.RuntimeCUDA}},
	} {
		if _, err := state.RegisterNode(node); err != nil {
			t.Fatal(err)
		}
	}
	low, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "low", Image: "work", Priority: 10, Requirements: model.Requirements{GPUCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	high, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "high", Image: "work", Priority: 90, Requirements: model.Requirements{GPUCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}

	page, err = state.ListSchedulingDecisions(SchedulingDecisionQuery{ProjectID: " alpha ", Limit: 1})
	if err != nil || page.Total != 2 || page.Limit != 1 || len(page.Items) != 1 {
		t.Fatalf("unexpected bounded decision page: %+v err=%v", page, err)
	}
	all, err := state.ListSchedulingDecisions(SchedulingDecisionQuery{ProjectID: "alpha", Limit: maxSchedulingDecisionLimit + 50})
	if err != nil || all.Total != 2 || all.Limit != maxSchedulingDecisionLimit || len(all.Items) != 2 {
		t.Fatalf("unexpected decision history: %+v err=%v", all, err)
	}
	byJob := map[string]*model.SchedulingDecision{}
	for _, decision := range all.Items {
		byJob[decision.JobID] = decision
	}
	highDecision, lowDecision := byJob[high.ID], byJob[low.ID]
	if highDecision == nil || lowDecision == nil {
		t.Fatalf("missing decisions: %+v", all.Items)
	}
	if highDecision.Algorithm != SchedulingAlgorithmWeightedFair || highDecision.ReasonCode != SchedulingReasonWeightedFairSelected ||
		highDecision.Priority != 90 || highDecision.ProjectWeight != 2 || highDecision.GPUCost != 1 ||
		highDecision.ProjectVRuntimeBefore != 0 || highDecision.ProjectVRuntimeAfter != fairSchedulingStride/2 ||
		highDecision.StrideDelta != fairSchedulingStride/2 || highDecision.NodeID != "cheap" ||
		highDecision.NodeVendor != model.VendorNVIDIA || highDecision.NodeRuntime != model.RuntimeCUDA ||
		highDecision.NodeModel != "A100" || highDecision.NodeVRAMGB != 40 || highDecision.NodeHourlyPrice != 1 ||
		highDecision.EligibleNodeCount != 2 || highDecision.RunnableProjectCount != 1 ||
		highDecision.JobCreatedAt.IsZero() || len(highDecision.AllocatedGPUs) != 1 {
		t.Fatalf("decision does not explain the assignment: %+v", highDecision)
	}
	if lowDecision.ProjectVRuntimeBefore != fairSchedulingStride/2 || lowDecision.ProjectVRuntimeAfter != fairSchedulingStride || lowDecision.EligibleNodeCount != 1 {
		t.Fatalf("second decision did not snapshot its own scheduling round: %+v", lowDecision)
	}

	explanation := mustSchedulingExplanation(t, state, high.ID)
	if explanation.ReasonCode != SchedulingReasonAssigned || explanation.LatestDecision == nil || explanation.LatestDecision.DecisionID != highDecision.DecisionID {
		t.Fatalf("assigned explanation omitted its durable decision: %+v", explanation)
	}
	highDecision.AllocatedGPUs[0] = 99
	again, _ := state.ListSchedulingDecisions(SchedulingDecisionQuery{JobID: high.ID})
	if again.Items[0].AllocatedGPUs[0] == 99 {
		t.Fatal("decision query exposed mutable store state")
	}

	assigned, _ := state.GetJob(high.ID)
	if _, err := state.UpdateJob(high.ID, assigned.AssignedNode, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.UpdateJob(high.ID, assigned.AssignedNode, model.JobUpdate{Status: model.JobSucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := state.BeginJobDeletion(high.ID); err != nil {
		t.Fatal(err)
	}
	if err := state.DeleteJob(high.ID); err != nil {
		t.Fatal(err)
	}
	retained, err := state.ListSchedulingDecisions(SchedulingDecisionQuery{ProjectID: "alpha", JobID: high.ID})
	if err != nil || retained.Total != 1 || len(retained.Items) != 1 {
		t.Fatalf("job deletion removed scheduling audit: %+v err=%v", retained, err)
	}
	empty, err := state.ListSchedulingDecisions(SchedulingDecisionQuery{ProjectID: "unknown", JobID: "unknown"})
	if err != nil || empty.Total != 0 || len(empty.Items) != 0 {
		t.Fatalf("unknown history filter did not return an empty page: %+v err=%v", empty, err)
	}
}

func TestQueuedSchedulingReasonsUseSchedulerPredicates(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T) (*Store, string)
		want  string
	}{
		{
			name: "no nodes",
			setup: func(t *testing.T) (*Store, string) {
				state := NewMemory()
				job, err := state.CreateJob(model.JobCreate{Name: "job", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
				if err != nil {
					t.Fatal(err)
				}
				return state, job.ID
			},
			want: SchedulingReasonNoRegisteredNodes,
		},
		{
			name: "requirements",
			setup: func(t *testing.T) (*Store, string) {
				state := NewMemory()
				if _, err := state.RegisterNode(model.Node{ID: "node", GPUModel: "A100", GPUCount: 1, VRAMGB: 40}); err != nil {
					t.Fatal(err)
				}
				job, err := state.CreateJob(model.JobCreate{Name: "job", Image: "work", Requirements: model.Requirements{GPUCount: 1, GPUModels: []string{"H100"}}})
				if err != nil {
					t.Fatal(err)
				}
				return state, job.ID
			},
			want: SchedulingReasonRequirementsUnmatched,
		},
		{
			name: "license",
			setup: func(t *testing.T) (*Store, string) {
				state := NewMemory()
				if _, err := state.RegisterNode(model.Node{ID: "node", GPUCount: 1, VRAMGB: 24}); err != nil {
					t.Fatal(err)
				}
				state.SetSchedulingLimits(0, 0, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339))
				job, err := state.CreateJob(model.JobCreate{Name: "job", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
				if err != nil {
					t.Fatal(err)
				}
				return state, job.ID
			},
			want: SchedulingReasonLicenseUnavailable,
		},
		{
			name: "not ready",
			setup: func(t *testing.T) (*Store, string) {
				state := NewMemory()
				if _, err := state.RegisterNodeSession(model.Node{ID: "node", GPUCount: 1, VRAMGB: 24}, "session"); err != nil {
					t.Fatal(err)
				}
				job, err := state.CreateJob(model.JobCreate{Name: "job", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
				if err != nil {
					t.Fatal(err)
				}
				return state, job.ID
			},
			want: SchedulingReasonNodesNotReady,
		},
		{
			name: "capacity",
			setup: func(t *testing.T) (*Store, string) {
				state := NewMemory()
				if _, err := state.RegisterNode(model.Node{ID: "node", GPUCount: 1, VRAMGB: 24}); err != nil {
					t.Fatal(err)
				}
				if _, err := state.CreateJob(model.JobCreate{Name: "first", Image: "work", Requirements: model.Requirements{GPUCount: 1}}); err != nil {
					t.Fatal(err)
				}
				if err := state.Schedule(time.Minute); err != nil {
					t.Fatal(err)
				}
				job, err := state.CreateJob(model.JobCreate{Name: "second", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
				if err != nil {
					t.Fatal(err)
				}
				return state, job.ID
			},
			want: SchedulingReasonInsufficientCapacity,
		},
		{
			name: "disabled project",
			setup: func(t *testing.T) (*Store, string) {
				state := NewMemory()
				createWeightedProject(t, state, "alpha", 1)
				job, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "job", Image: "work"})
				if err != nil {
					t.Fatal(err)
				}
				disabled := model.ProjectDisabled
				if _, err := state.UpdateProject("alpha", model.ProjectUpdate{Status: &disabled}); err != nil {
					t.Fatal(err)
				}
				return state, job.ID
			},
			want: SchedulingReasonProjectDisabled,
		},
		{
			name: "concurrency quota",
			setup: func(t *testing.T) (*Store, string) {
				state := NewMemory()
				if _, err := state.CreateProject(model.ProjectCreate{ID: "alpha", Name: "alpha", MaxConcurrentJobs: 1, MaxGPUs: 2}); err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"node-a", "node-b"} {
					if _, err := state.RegisterNode(model.Node{ID: id, GPUCount: 1, VRAMGB: 24}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "first", Image: "work", Requirements: model.Requirements{GPUCount: 1}}); err != nil {
					t.Fatal(err)
				}
				if err := state.Schedule(time.Minute); err != nil {
					t.Fatal(err)
				}
				job, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "second", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
				if err != nil {
					t.Fatal(err)
				}
				return state, job.ID
			},
			want: ProjectQuotaConcurrency,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, jobID := test.setup(t)
			explanation := mustSchedulingExplanation(t, state, jobID)
			if explanation.ReasonCode != test.want {
				t.Fatalf("reason=%q want %q: %+v", explanation.ReasonCode, test.want, explanation)
			}
		})
	}
}

func TestSchedulingExplanationShowsPriorityAndFairOrderWithoutMutation(t *testing.T) {
	state := NewMemory()
	state.SetProjectFairScheduling(true)
	createWeightedProject(t, state, "alpha", 1)
	createWeightedProject(t, state, "beta", 1)
	if _, err := state.RegisterNode(model.Node{ID: "worker", GPUCount: 1, VRAMGB: 24}); err != nil {
		t.Fatal(err)
	}
	low, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "low", Image: "work", Priority: 1, Requirements: model.Requirements{GPUCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	high, err := state.CreateJobForProject("alpha", model.JobCreate{Name: "high", Image: "work", Priority: 100, Requirements: model.Requirements{GPUCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	beta, err := state.CreateJobForProject("beta", model.JobCreate{Name: "beta", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	if err != nil {
		t.Fatal(err)
	}

	lowExplanation := mustSchedulingExplanation(t, state, low.ID)
	if lowExplanation.ReasonCode != SchedulingReasonProjectQueueOrder || lowExplanation.JobsAheadInProject != 1 {
		t.Fatalf("priority order was not explained: %+v", lowExplanation)
	}
	if current, _ := state.GetJob(low.ID); current.Status != model.JobQueued {
		t.Fatalf("inspection mutated job: %+v", current)
	}

	state.mu.Lock()
	state.state.Projects["alpha"].SchedulerVRuntime = 100
	state.state.Projects["beta"].SchedulerVRuntime = 0
	state.mu.Unlock()
	// The high-priority job is alpha's project candidate, but beta has the
	// lower project virtual runtime and therefore wins the next fair turn.
	highExplanation := mustSchedulingExplanation(t, state, high.ID)
	if highExplanation.ReasonCode != SchedulingReasonFairShareWait || highExplanation.RelativeProjectVRuntime != 100 {
		t.Fatalf("fair-share order was not explained: %+v", highExplanation)
	}
	betaExplanation := mustSchedulingExplanation(t, state, beta.ID)
	if betaExplanation.ReasonCode != SchedulingReasonAwaitingDispatch || betaExplanation.RelativeProjectVRuntime != 0 {
		t.Fatalf("next fair candidate was not explained: %+v", betaExplanation)
	}

	states := state.ListProjectSchedulingStates()
	byProject := map[string]model.ProjectSchedulingState{}
	for _, project := range states {
		byProject[project.ProjectID] = project
	}
	if byProject["alpha"].RelativeVRuntime != 100 || byProject["alpha"].QueuedJobs != 2 ||
		byProject["beta"].RelativeVRuntime != 0 || byProject["beta"].QueuedJobs != 1 {
		t.Fatalf("unexpected project scheduling state: %+v", states)
	}
}

func TestFIFOExplanationUsesCreationOrderInsteadOfPriority(t *testing.T) {
	state := NewMemory()
	if _, err := state.RegisterNode(model.Node{ID: "worker", GPUCount: 1, VRAMGB: 24}); err != nil {
		t.Fatal(err)
	}
	older, err := state.CreateJob(model.JobCreate{Name: "older", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := state.CreateJob(model.JobCreate{Name: "newer", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.state.Jobs[older.ID].Priority = MinJobPriority
	state.state.Jobs[newer.ID].Priority = MaxJobPriority
	state.mu.Unlock()

	explanation := mustSchedulingExplanation(t, state, newer.ID)
	if explanation.SchedulerMode != SchedulingAlgorithmFIFO || explanation.ReasonCode != SchedulingReasonProjectQueueOrder || explanation.JobsAheadInProject != 1 {
		t.Fatalf("FIFO explanation incorrectly applied priority order: %+v", explanation)
	}
}
