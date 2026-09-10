package api

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuflow/internal/artifact"
	"gpuflow/internal/model"
	"gpuflow/internal/store"
	"gpuflow/pkg/edition"
)

// publicationS3 implements the small S3 surface exercised by the real MinIO
// artifact store. Its first CopyObject can stall independently of API requests.
type publicationS3 struct {
	mu           sync.Mutex
	objects      map[string][]byte
	copyStarted  chan struct{}
	releaseCopy  chan struct{}
	copyCount    int
	failCopyName string
}

func (s *publicationS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/artifacts/")
	if r.URL.Path == "/artifacts/" || r.URL.Path == "/artifacts" {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Query().Has("location") {
			io.WriteString(w, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`)
			return
		}
		type entry struct {
			Key          string
			LastModified string
			ETag         string
			Size         int
		}
		listing := struct {
			XMLName     xml.Name `xml:"ListBucketResult"`
			Name        string
			Prefix      string
			IsTruncated bool
			Contents    []entry
		}{Name: "artifacts", Prefix: r.URL.Query().Get("prefix")}
		s.mu.Lock()
		for name, payload := range s.objects {
			if strings.HasPrefix(name, listing.Prefix) {
				listing.Contents = append(listing.Contents, entry{name, time.Now().UTC().Format(time.RFC3339), `"` + publicationETag(payload) + `"`, len(payload)})
			}
		}
		s.mu.Unlock()
		sort.Slice(listing.Contents, func(i, j int) bool { return listing.Contents[i].Key < listing.Contents[j].Key })
		w.Header().Set("Content-Type", "application/xml")
		xml.NewEncoder(w).Encode(listing)
		return
	}
	if source := r.Header.Get("X-Amz-Copy-Source"); source != "" {
		s.mu.Lock()
		s.copyCount++
		first := s.copyCount == 1
		failCopy := s.failCopyName != "" && strings.HasSuffix(key, "/"+s.failCopyName)
		s.mu.Unlock()
		if first {
			close(s.copyStarted)
			select {
			case <-s.releaseCopy:
			case <-r.Context().Done():
				return
			}
		}
		if failCopy {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>injected copy failure</Message></Error>`)
			return
		}
		source, _ = url.PathUnescape(source)
		source = strings.TrimPrefix(strings.TrimPrefix(source, "/"), "artifacts/")
		s.mu.Lock()
		payload, exists := s.objects[source]
		s.objects[key] = append([]byte(nil), payload...)
		s.mu.Unlock()
		if !exists {
			http.Error(w, "missing source", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<CopyObjectResult><ETag>"%s"</ETag><LastModified>%s</LastModified></CopyObjectResult>`, publicationETag(payload), time.Now().UTC().Format(time.RFC3339))
		return
	}
	if r.Method == http.MethodPut {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// MinIO's HTTP streaming signature wraps small PUTs in AWS chunks.
		if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") || strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			var decoded bytes.Buffer
			for len(payload) > 0 {
				index := bytes.Index(payload, []byte("\r\n"))
				if index < 0 {
					break
				}
				var size int
				fmt.Sscanf(strings.Split(string(payload[:index]), ";")[0], "%x", &size)
				payload = payload[index+2:]
				if size == 0 || size > len(payload) {
					break
				}
				decoded.Write(payload[:size])
				payload = payload[size+2:]
			}
			payload = decoded.Bytes()
		}
		s.mu.Lock()
		s.objects[key] = append([]byte(nil), payload...)
		s.mu.Unlock()
		w.Header().Set("ETag", `"`+publicationETag(payload)+`"`)
		return
	}
	if r.Method == http.MethodDelete {
		s.mu.Lock()
		delete(s.objects, key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mu.Lock()
	payload, exists := s.objects[key]
	payload = append([]byte(nil), payload...)
	s.mu.Unlock()
	if !exists {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
		return
	}
	w.Header().Set("ETag", `"`+publicationETag(payload)+`"`)
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
	if r.Method != http.MethodHead {
		w.Write(payload)
	}
}

func publicationETag(payload []byte) string {
	hash := md5.Sum(payload)
	return hex.EncodeToString(hash[:])
}

func publicationUpload(t *testing.T, server *httptest.Server, jobID, nodeID, session, token, content string) *http.Request {
	t.Helper()
	return publicationUploadFile(t, server, jobID, nodeID, session, token, "training.log", content)
}

func publicationUploadFile(t *testing.T, server *httptest.Server, jobID, nodeID, session, token, name, content string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(part, content); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/jobs/"+jobID+"/artifacts?node_id="+nodeID, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set(model.HeaderAgentSession, session)
	req.Header.Set(model.HeaderAttemptToken, token)
	return req
}

func TestArtifactSlowCopyDoesNotBlockHeartbeatsAndStaleCopyCannotPublish(t *testing.T) {
	backend := &publicationS3{objects: map[string][]byte{}, copyStarted: make(chan struct{}), releaseCopy: make(chan struct{})}
	s3 := httptest.NewServer(backend)
	defer s3.Close()
	artifacts, err := artifact.Open(artifact.Config{Endpoint: strings.TrimPrefix(s3.URL, "http://"), AccessKey: "test-access", SecretKey: "test-secret", Bucket: "artifacts", Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	state := store.NewMemory()
	server := httptest.NewServer(NewWithStores(state, state, artifacts, "test-token", edition.Community()).Handler())
	defer server.Close()
	node, err := state.RegisterNodeSession(model.Node{ID: "copy-node"}, "session-one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = state.ConfirmNodeCleanupSession(node.ID, "session-one"); err != nil {
		t.Fatal(err)
	}
	other, err := state.RegisterNodeSession(model.Node{ID: "other-node"}, "other-session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = state.ConfirmNodeCleanupSession(other.ID, "other-session"); err != nil {
		t.Fatal(err)
	}
	job, err := state.CreateJob(model.JobCreate{Name: "copy", Image: "work", MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	dispatch, err := state.NextJobSession(node.ID, "session-one")
	if err != nil || dispatch == nil {
		t.Fatalf("dispatch: %+v %v", dispatch, err)
	}
	if _, err = state.UpdateJobLease(job.ID, node.ID, "session-one", dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	backend.objects[job.ID+"/training.log"] = []byte("legacy log")
	backend.objects[job.ID+"/legacy.txt"] = []byte("legacy file")
	backend.mu.Unlock()
	req := publicationUpload(t, server, job.ID, node.ID, "session-one", dispatch.AttemptToken, "stale log")
	type uploadResult struct {
		response *http.Response
		err      error
	}
	result := make(chan uploadResult, 1)
	go func() { response, err := http.DefaultClient.Do(req); result <- uploadResult{response, err} }()
	released := false
	defer func() {
		if !released {
			close(backend.releaseCopy)
		}
	}()
	select {
	case <-backend.copyStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("copy did not start")
	}
	for _, credentials := range [][2]string{{node.ID, "session-one"}, {other.ID, "other-session"}} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		heartbeat, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/nodes/"+credentials[0]+"/heartbeat", nil)
		heartbeat.Header.Set("Authorization", "Bearer test-token")
		heartbeat.Header.Set(model.HeaderAgentSession, credentials[1])
		response, err := http.DefaultClient.Do(heartbeat)
		cancel()
		if err != nil {
			t.Fatalf("slow copy blocked %s heartbeat: %v", credentials[0], err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("heartbeat returned %d", response.StatusCode)
		}
	}
	// Take over while the old HTTP CopyObject is still blocked, then publish a
	// replacement attempt's output before allowing the stale copy to finish.
	replacement, err := state.RegisterNode(model.Node{ID: node.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = state.UpdateJob(job.ID, node.ID, model.JobUpdate{Status: model.JobCanceled}); err != nil {
		t.Fatal(err)
	}
	if err = state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	newDispatch, err := state.NextJobSession(node.ID, replacement.SessionEpoch)
	if err != nil || newDispatch == nil {
		t.Fatalf("retry dispatch: %+v %v", newDispatch, err)
	}
	if _, err = state.UpdateJobLease(job.ID, node.ID, replacement.SessionEpoch, newDispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(publicationUpload(t, server, job.ID, node.ID, replacement.SessionEpoch, newDispatch.AttemptToken, "replacement log"))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("replacement upload: %d %s", response.StatusCode, responseBody)
	}
	close(backend.releaseCopy)
	released = true
	select {
	case stale := <-result:
		if stale.err != nil {
			t.Fatal(stale.err)
		}
		defer stale.response.Body.Close()
		if stale.response.StatusCode != http.StatusConflict {
			t.Fatalf("stale upload returned %d", stale.response.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale upload did not finish")
	}
	for endpoint, want := range map[string]string{"/artifacts/training.log": "replacement log", "/logs/full": "replacement log"} {
		get, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/jobs/"+job.ID+endpoint, nil)
		get.Header.Set("Authorization", "Bearer test-token")
		response, err := http.DefaultClient.Do(get)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || string(payload) != want {
			t.Fatalf("%s returned %d %q", endpoint, response.StatusCode, payload)
		}
	}
	if status := request(t, server, http.MethodGet, "/v1/jobs/"+job.ID+"/artifacts/legacy.txt", nil, nil); status != http.StatusNotFound {
		t.Fatalf("unattributed legacy artifact returned %d after retry", status)
	}
	var listed struct {
		Items []artifact.Item `json:"items"`
	}
	if status := request(t, server, http.MethodGet, "/v1/jobs/"+job.ID+"/artifacts", nil, &listed); status != http.StatusOK || len(listed.Items) != 1 || listed.Items[0].Name != "training.log" {
		t.Fatalf("listing: %d %+v", status, listed)
	}
	for _, item := range listed.Items {
		if strings.Contains(item.Name, ".gpuflow-") {
			t.Fatalf("internal object leaked: %+v", item)
		}
	}
	if _, err = state.UpdateJobLease(job.ID, node.ID, replacement.SessionEpoch, newDispatch.AttemptToken, model.JobUpdate{Status: model.JobSucceeded}); err != nil {
		t.Fatal(err)
	}
	if status := request(t, server, http.MethodDelete, "/v1/jobs/"+job.ID, nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete returned %d", status)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	for name := range backend.objects {
		if strings.HasPrefix(name, job.ID+"/") {
			t.Fatalf("job deletion retained object %s", name)
		}
	}
}
