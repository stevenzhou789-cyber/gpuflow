// Package projectscope carries the authenticated project boundary through an
// HTTP request. The value is deliberately stored under a private context key:
// callers must construct it after authentication rather than from request
// parameters supplied by a client.
package projectscope

import (
	"context"
	"strings"
)

const DefaultProjectID = "default"

// Scope identifies either one authenticated project or a trusted platform
// administrator that may operate across all projects. The zero value is the
// Community-compatible default-project scope; it is never global.
type Scope struct {
	ProjectID   string
	AllProjects bool
}

func Default() Scope { return Scope{ProjectID: DefaultProjectID} }

func Project(projectID string) Scope {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = DefaultProjectID
	}
	return Scope{ProjectID: projectID}
}

func All() Scope { return Scope{AllProjects: true} }

// Allows reports whether the authenticated scope may access a persisted
// project. Empty persisted IDs are treated as legacy default-project data.
func (s Scope) Allows(projectID string) bool {
	if s.AllProjects {
		return true
	}
	want := strings.TrimSpace(s.ProjectID)
	if want == "" {
		want = DefaultProjectID
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = DefaultProjectID
	}
	return projectID == want
}

type contextKey struct{}

func WithContext(ctx context.Context, scope Scope) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, contextKey{}, scope)
}

func FromContext(ctx context.Context) (Scope, bool) {
	if ctx == nil {
		return Default(), false
	}
	scope, ok := ctx.Value(contextKey{}).(Scope)
	if !ok {
		return Default(), false
	}
	if !scope.AllProjects && strings.TrimSpace(scope.ProjectID) == "" {
		scope.ProjectID = DefaultProjectID
	}
	return scope, true
}
