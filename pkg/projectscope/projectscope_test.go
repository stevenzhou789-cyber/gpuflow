package projectscope

import (
	"context"
	"testing"
)

func TestScopeDefaultsFailClosedToDefaultProject(t *testing.T) {
	if (Scope{}).Allows("tenant") || !(Scope{}).Allows(DefaultProjectID) {
		t.Fatal("zero scope did not fail closed to the default project")
	}
	if scope, ok := FromContext(context.Background()); ok || !scope.Allows(DefaultProjectID) {
		t.Fatalf("missing context scope was not default-only: %+v ok=%v", scope, ok)
	}
}

func TestAuthenticatedScopesRoundTrip(t *testing.T) {
	project := Project("alpha")
	ctx := WithContext(context.Background(), project)
	restored, ok := FromContext(ctx)
	if !ok || !restored.Allows("alpha") || restored.Allows("beta") {
		t.Fatalf("project scope did not round trip: %+v ok=%v", restored, ok)
	}
	if !All().Allows("alpha") || !All().Allows("beta") {
		t.Fatal("global scope did not allow every project")
	}
}
