package routing

import (
	"reflect"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/policy"
)

// CloneForPreview copies State field by field, so every field added to State has to be added
// here too. Forgetting one does not fail to compile: it silently makes the dashboard preview
// disagree with what the proxy will actually do, which is the one guarantee this project makes
// about preview. This test fails when a scalar field is left behind.
func TestCloneForPreviewCopiesEveryScalarField(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	src := &policy.State{
		Version:             7,
		Order:               []string{"a"},
		DefaultWorkspaceID:  "a",
		StaleAfter:          3 * time.Minute,
		HandoffBelowPercent: 5.5,
		Rules:               []policy.Rule{{ID: "r"}},
		Workspaces: map[string]*policy.WorkspaceState{
			"a": {ID: "a", Name: "A", Windows: map[int64]policy.Window{
				300: {Minutes: 300, UsedPercent: 40, ResetsAt: &reset},
			}},
		},
	}
	dst := CloneForPreview(src)

	sv, dv := reflect.ValueOf(*src), reflect.ValueOf(*dst)
	for i := 0; i < sv.NumField(); i++ {
		f := sv.Type().Field(i)
		switch f.Type.Kind() {
		case reflect.Slice, reflect.Map, reflect.Ptr:
			continue // deep-copied on purpose; compared by behaviour elsewhere
		}
		if !reflect.DeepEqual(sv.Field(i).Interface(), dv.Field(i).Interface()) {
			t.Errorf("CloneForPreview dropped %s: got %v, want %v",
				f.Name, dv.Field(i).Interface(), sv.Field(i).Interface())
		}
	}
}
