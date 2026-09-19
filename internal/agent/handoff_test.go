package agent

import (
	"context"
	"encoding/json"
	"gpuflow/internal/model"
	"gpuflow/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHandoffWaitsForTerminalAcknowledgement(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/next") {
			json.NewEncoder(w).Encode(model.AgentJob{Job: model.Job{ID: "job-1", Status: model.JobAssigned, Attempts: 1}, AttemptToken: "fixture-attempt"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/status") {
			var u model.JobUpdate
			json.NewDecoder(r.Body).Decode(&u)
			if u.Status == model.JobSucceeded {
				close(entered)
				<-release
			}
			w.WriteHeader(200)
			return
		}
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(model.Job{ID: "job-1", Status: model.JobRunning})
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	a := New(Config{Server: server.URL, ID: "gpu-1", Executor: "mock"})
	a.ready = true
	a.setSessionLeaseDeadline(time.Now().Add(time.Minute))
	tickDone := make(chan error, 1)
	go func() { tickDone <- a.tick(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("did not reach terminal acknowledgement")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	response := httptest.NewRecorder()
	a.localControlHandler(context.Background()).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/quiesce", nil).WithContext(ctx))
	if response.Body.String() == "ok\n" {
		t.Fatal("quiesce acknowledged before terminal status persisted")
	}
	close(release)
	if err := <-tickDone; err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	a.localControlHandler(context.Background()).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/quiesce", nil))
	if response.Body.String() != "ok\n" {
		t.Fatal("completed attempt did not drain")
	}
}

func TestLocalHandoffSocketOwnershipAndResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("CE container handoff is a Linux Unix socket")
	}
	socket := filepath.Join(t.TempDir(), "handoff.sock")
	a := New(Config{LocalControl: socket})
	a.ready = true
	a.setSessionLeaseDeadline(time.Now().Add(time.Minute))
	closeControl, err := a.serveLocalControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private socket permissions: %v %v", info, err)
	}
	b := New(Config{LocalControl: socket})
	if closePeer, err := b.serveLocalControl(context.Background()); err == nil {
		closePeer()
		t.Fatal("replaced live socket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, action := range []string{"ready", "quiesce", "resume", "ready"} {
		if err := LocalControl(ctx, socket, action); err != nil {
			t.Fatal(action, err)
		}
	}
}

func TestHandoffWaitsForClaimAndStopsNewClaims(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	a := New(Config{Server: server.URL, ID: "gpu-01"})
	a.ready = true
	a.setSessionLeaseDeadline(time.Now().Add(time.Minute))
	tickDone := make(chan error, 1)
	go func() { tickDone <- a.tick(context.Background()) }()
	<-entered
	request := httptest.NewRequest(http.MethodPost, "/quiesce", nil)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { a.localControlHandler(context.Background()).ServeHTTP(response, request); close(done) }()
	deadline := time.Now().Add(time.Second)
	for {
		a.handoffMu.Lock()
		draining := a.draining
		a.handoffMu.Unlock()
		if draining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handoff did not stop claims")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("handoff acknowledged while /next was still in flight")
	default:
	}
	if e := a.tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 1 {
		t.Fatal("quiesced Agent issued another claim")
	}
	close(release)
	if e := <-tickDone; e != nil {
		t.Fatal(e)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handoff did not finish")
	}
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	// Claim exclusion remains active after the acknowledgement until replacement.
	if e := a.tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 1 {
		t.Fatal("claims resumed after handoff acknowledgement")
	}
	a.setSessionLeaseDeadline(time.Now().Add(-time.Second))
	response = httptest.NewRecorder()
	a.localControlHandler(context.Background()).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/ready", nil))
	if response.Code == 200 {
		t.Fatal("expired session reported ready")
	}
}

func TestRejectUnsafeNodeIDsBeforeRegistration(t *testing.T) {
	for _, id := range []string{"gpu/01", "gpu&lab", "gpu?01", "..", " gpu", "gpu\n01"} {
		s := store.NewMemory()
		if _, e := s.RegisterNode(model.Node{ID: id}); e == nil {
			t.Fatalf("registered unsafe ID %q", id)
		}
		if len(s.ListNodes()) != 0 {
			t.Fatal("invalid registration changed store")
		}
		a := New(Config{ID: id})
		if e := a.Run(context.Background()); e == nil {
			t.Fatalf("Agent accepted %q", id)
		}
	}
	if !model.ValidNodeID("gpu-01_pool.a") {
		t.Fatal("normal ID rejected")
	}
}
