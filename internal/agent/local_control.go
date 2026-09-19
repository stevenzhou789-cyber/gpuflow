package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"
)

// This socket is accessible only to the local process owner. It is an installer
// handoff, not a control-plane maintenance API or an edition capability.
func (a *Agent) serveLocalControl(ctx context.Context) (func(), error) {
	if a.cfg.LocalControl == "" {
		return func() {}, nil
	}
	// A killed process can leave its private socket inode behind. Remove only
	// a socket that the kernel proves has no listener, never a file or live peer.
	if info, statErr := os.Lstat(a.cfg.LocalControl); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("Agent handoff path is not a socket")
		}
		peer, dialErr := net.DialTimeout("unix", a.cfg.LocalControl, time.Second)
		if dialErr == nil {
			peer.Close()
			return nil, errors.New("another Agent owns the handoff socket")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, errors.New("cannot establish stale Agent handoff socket")
		}
		if err := os.Remove(a.cfg.LocalControl); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(statErr) {
		return nil, statErr
	}
	listener, err := net.Listen("unix", a.cfg.LocalControl)
	if err != nil {
		return nil, fmt.Errorf("local Agent handoff: %w", err)
	}
	if err = os.Chmod(a.cfg.LocalControl, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: a.localControlHandler(ctx)}
	go func() { _ = server.Serve(listener) }()
	return func() { _ = server.Close() }, nil
}

func (a *Agent) localControlHandler(ctx context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		a.handoffMu.Lock()
		switch r.URL.Path {
		case "/quiesce":
			a.draining = true
		case "/resume":
			a.draining = false
		case "/ready":
		default:
			a.handoffMu.Unlock()
			http.NotFound(w, r)
			return
		}
		a.handoffMu.Unlock()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			a.handoffMu.Lock()
			valid := time.Now().Before(a.sessionLeaseDeadline()) && ctx.Err() == nil
			ready := a.ready
			drained := a.draining && a.activeTicks == 0
			paused := a.draining
			a.handoffMu.Unlock()
			if !valid || (r.URL.Path == "/ready" && !ready) {
				http.Error(w, "Agent session is not ready", 503)
				return
			}
			if r.URL.Path == "/resume" || (r.URL.Path == "/ready" && !paused) || (r.URL.Path == "/quiesce" && drained) {
				_, _ = io.WriteString(w, "ok\n")
				return
			}
			if r.URL.Path == "/ready" {
				http.Error(w, "Agent is quiesced", 503)
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

func LocalControl(ctx context.Context, socket, action string) error {
	if action != "quiesce" && action != "resume" && action != "ready" {
		return errors.New("expected quiesce, resume or ready")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/"+action, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Agent handoff %s failed: HTTP %d", action, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16))
	if err != nil || string(body) != "ok\n" {
		return errors.New("invalid Agent handoff acknowledgement")
	}
	return nil
}
