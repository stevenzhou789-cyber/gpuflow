package store

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gpuflow/internal/model"
)

func TestJobOutputPersistenceKeepsUTF8Tail(t *testing.T) {
	// Reproduce the Agent's byte-limited buffer followed by UTF-8 repair.
	raw := strings.Repeat("中", 21846)
	agentOutput := strings.ToValidUTF8(raw[len(raw)-maxJobOutputBytes:], "\uFFFD")
	if !utf8.ValidString(agentOutput) || len(agentOutput) <= maxJobOutputBytes {
		t.Fatal("fixture must be valid UTF-8 expanded beyond the byte limit")
	}
	for _, test := range []struct {
		name, output, want string
	}{
		{"agent_chinese_tail", agentOutput, strings.Repeat("中", 21845)},
		{"ascii_tail", strings.Repeat("a", maxJobOutputBytes) + "end", strings.Repeat("a", maxJobOutputBytes-3) + "end"},
		{"emoji_tail", strings.Repeat("🙂", maxJobOutputBytes/4) + "end", strings.Repeat("🙂", maxJobOutputBytes/4-1) + "end"},
		{"short_log", "开始训练\n", "开始训练\n"},
		{"invalid_bytes", "start\xff\xfeend", "start\uFFFDend"},
	} {
		for _, terminal := range []bool{false, true} {
			mode := "live"
			if terminal {
				mode = "finished"
			}
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				s := NewMemory()
				if _, err := s.RegisterNode(model.Node{ID: "log-node", GPUCount: 1, VRAMGB: 24}); err != nil {
					t.Fatal(err)
				}
				job, err := s.CreateJob(model.JobCreate{Name: "log-test", Image: "work", Requirements: model.Requirements{GPUCount: 1}})
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Schedule(time.Minute); err != nil {
					t.Fatal(err)
				}
				if _, err := s.UpdateJob(job.ID, "log-node", model.JobUpdate{Status: model.JobRunning}); err != nil {
					t.Fatal(err)
				}
				if terminal {
					_, err = s.UpdateJob(job.ID, "log-node", model.JobUpdate{Status: model.JobSucceeded, Output: test.output})
				} else {
					_, err = s.UpdateJobOutput(job.ID, "log-node", test.output)
				}
				if err != nil {
					t.Fatal(err)
				}
				stored, err := s.GetJob(job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !utf8.ValidString(stored.Output) || len(stored.Output) > maxJobOutputBytes || stored.Output != test.want {
					t.Fatalf("unexpected log tail: valid=%v bytes=%d want_bytes=%d", utf8.ValidString(stored.Output), len(stored.Output), len(test.want))
				}
			})
		}
	}
}
