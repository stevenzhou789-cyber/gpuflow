package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuflow/internal/artifact"
	"gpuflow/internal/model"
	"gpuflow/internal/store"
	"gpuflow/pkg/edition"
)

type countingArtifactStore struct {
	artifact.Store
	bytes        int64
	size         int64
	commits      int
	discards     int
	ignoreErrors bool
}

func (s *countingArtifactStore) Enabled() bool { return true }
func (s *countingArtifactStore) Stage(_ context.Context, _, _ string, r io.Reader, size int64) (artifact.Staged, error) {
	s.size = size
	n, err := io.Copy(io.Discard, r)
	s.bytes = n
	if s.ignoreErrors {
		err = nil // exercise SDKs that mistake a short source for normal EOF
	}
	return artifact.Staged{}, err
}
func (s *countingArtifactStore) Commit(context.Context, artifact.Staged) error {
	s.commits++
	return nil
}
func (s *countingArtifactStore) Discard(context.Context, artifact.Staged) error {
	s.discards++
	return nil
}

// zeroArtifactReader generates arbitrary lengths without allocating the file.
type zeroArtifactReader struct{ remaining int64 }

func (z *zeroArtifactReader) Read(p []byte) (int, error) {
	if z.remaining == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), z.remaining))
	clear(p[:n])
	z.remaining -= int64(n)
	return n, nil
}

func artifactUploadHarness(t *testing.T, backend artifact.Store) (http.Handler, *store.Store, *model.AgentJob) {
	t.Helper()
	state := store.NewMemory()
	if _, err := state.RegisterNodeSession(model.Node{ID: "stream-node"}, "stream-session"); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ConfirmNodeCleanupSession("stream-node", "stream-session"); err != nil {
		t.Fatal(err)
	}
	job, err := state.CreateJob(model.JobCreate{Name: "stream-test", Image: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	dispatch, err := state.NextJobSession("stream-node", "stream-session")
	if err != nil || dispatch == nil || dispatch.ID != job.ID {
		t.Fatalf("dispatch: %+v %v", dispatch, err)
	}
	if _, err := state.UpdateJobLease(job.ID, "stream-node", "stream-session", dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatal(err)
	}
	return NewWithStores(state, state, backend, "test-token", edition.Community()).Handler(), state, dispatch
}

func streamingArtifactRequest(job *model.AgentJob, input io.Reader, length int64) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+job.ID+"/artifacts?node_id=stream-node", input)
	r.ContentLength = length
	r.Header.Set("Authorization", "Bearer test-token")
	r.Header.Set(model.HeaderAgentSession, "stream-session")
	r.Header.Set(model.HeaderAttemptToken, job.AttemptToken)
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("Content-Disposition", `attachment; filename="weights.tar.gz"`)
	return r
}

func TestArtifactUploadStreamsBeyondOneGiB(t *testing.T) {
	const length int64 = (1 << 30) + 257
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_multipart=%v", legacy), func(t *testing.T) {
			backend := &countingArtifactStore{Store: artifact.Disabled()}
			handler, state, job := artifactUploadHarness(t, backend)
			var reader io.Reader = &zeroArtifactReader{remaining: length}
			r := streamingArtifactRequest(job, reader, length)
			if legacy {
				var envelope bytes.Buffer
				writer := multipart.NewWriter(&envelope)
				if _, err := writer.CreateFormFile("file", "weights.tar.gz"); err != nil {
					t.Fatal(err)
				}
				header := append([]byte(nil), envelope.Bytes()...)
				envelope.Reset()
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				reader = io.MultiReader(bytes.NewReader(header), reader, bytes.NewReader(envelope.Bytes()))
				r.Body = io.NopCloser(reader)
				r.ContentLength += int64(len(header) + envelope.Len())
				r.Header.Set("Content-Type", writer.FormDataContentType())
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusCreated || backend.bytes != length || backend.commits != 1 {
				t.Fatalf("upload: status=%d bytes=%d commits=%d body=%s", w.Code, backend.bytes, backend.commits, w.Body)
			}
			if r.MultipartForm != nil {
				t.Fatal("upload parsed/spooled the multipart form instead of streaming")
			}
			saved, err := state.GetJob(job.ID)
			if err != nil || saved.ArtifactRefs["weights.tar.gz"].Size != length {
				t.Fatalf("published reference: %+v, err=%v", saved, err)
			}
		})
	}
}

type interruptedArtifactReader struct{ sent bool }

func (r *interruptedArtifactReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "partial"), nil
	}
	return 0, io.ErrUnexpectedEOF
}

func TestArtifactUploadRejectsIncompleteOrOversizeWithoutPublication(t *testing.T) {
	for _, tc := range []struct {
		name, multipart string
		input           io.Reader
		length          int64
		wantStatus      int
	}{
		{name: "declared_short", input: strings.NewReader("short"), length: 10, wantStatus: http.StatusBadRequest},
		{name: "declared_long", input: strings.NewReader("too long"), length: 2, wantStatus: http.StatusBadRequest},
		{name: "interrupted", input: &interruptedArtifactReader{}, length: 100, wantStatus: http.StatusBadRequest},
		{name: "oversize", input: strings.NewReader("unread"), length: artifact.MaxSize + 1, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "unknown_raw_length", input: strings.NewReader("unread"), length: -1, wantStatus: http.StatusBadRequest},
		{name: "missing_multipart_end", input: strings.NewReader("--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"weights.tar.gz\"\r\n\r\npartial"), length: -1, multipart: "multipart/form-data; boundary=b", wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &countingArtifactStore{Store: artifact.Disabled(), ignoreErrors: true}
			handler, state, job := artifactUploadHarness(t, backend)
			r := streamingArtifactRequest(job, tc.input, tc.length)
			if tc.multipart != "" {
				r.Header.Set("Content-Type", tc.multipart)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.wantStatus || backend.commits != 0 {
				t.Fatalf("status=%d commits=%d body=%s", w.Code, backend.commits, w.Body)
			}
			saved, err := state.GetJob(job.ID)
			if err != nil || len(saved.ArtifactRefs) != 0 {
				t.Fatalf("incomplete upload published: %+v, err=%v", saved, err)
			}
		})
	}
}

func TestArtifactFileLimitIndependentOfMultipartEnvelope(t *testing.T) {
	r := &artifactUploadReader{input: strings.NewReader("extra"), bytes: artifact.MaxSize}
	_, err := io.ReadAll(r)
	if !errors.Is(err, errArtifactTooLarge) {
		t.Fatalf("artifact size cap returned %v", err)
	}
}
