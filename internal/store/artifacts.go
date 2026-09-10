package store

import (
	"errors"
	"path"
	"strings"

	"gpuflow/internal/model"
)

func cloneArtifactReferences(source map[string]model.ArtifactReference) map[string]model.ArtifactReference {
	if source == nil {
		return nil
	}
	result := make(map[string]model.ArtifactReference, len(source))
	for name, reference := range source {
		result[name] = reference
	}
	return result
}

// PublishJobArtifact is the only visibility change for an immutable upload.
// All object-store I/O completes before this call. Publication and Agent fencing
// share Store.mu and the normal state transaction, so a late stale upload cannot
// replace the visible reference, while slow S3 copies never block heartbeats.
func (s *Store) PublishJobArtifact(id, nodeID, session, attemptToken, name string, reference model.ArtifactReference) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateJobAttemptLocked(id, nodeID, session, attemptToken); err != nil {
		return err
	}
	if name == "" || name == "." || name == ".." || name != path.Base(name) || strings.Contains(name, "\\") ||
		!strings.HasPrefix(reference.StorageID, id+"/.gpuflow-versions/") || reference.StorageID != path.Clean(reference.StorageID) || reference.Size < 0 {
		return errors.New("invalid artifact reference")
	}
	before := cloneSnapshot(s.state)
	job := s.state.Jobs[id]
	// Never trust caller-supplied attribution. Validation, attribution, and
	// publication use the same lock and durable transaction as job transitions.
	reference.Attempt = job.Attempts
	// Copy on write also keeps previously returned Job snapshots immutable.
	job.ArtifactRefs = cloneArtifactReferences(job.ArtifactRefs)
	if job.ArtifactRefs == nil {
		job.ArtifactRefs = make(map[string]model.ArtifactReference)
	}
	job.ArtifactRefs[name] = reference
	return s.commitLocked(before)
}
