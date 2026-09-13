package service

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/proxy"
	"github.com/lgoyal6/codex-relay/internal/secrets"
)

// No list the dashboard iterates may serialise as null.
//
// This has now black-screened the app twice from different fields, so the guarantee is
// asserted over the WHOLE serialised row by reflection rather than field by field: any slice
// added later is covered without anyone remembering to come back here.
//
// The row used is the worst case on purpose, a record with no decision attached, which is
// what an authentication refusal produces.
func TestNoActivityListEverSerialisesAsNull(t *testing.T) {
	svc := connectService(t, secrets.NewMemory())
	ctx := context.Background()

	// Deliberately minimal: no Decision, no workspace, no usage.
	if err := svc.writeDecision(proxy.Record{
		At:         time.Now().UTC(),
		Attempt:    1,
		StatusCode: 401,
		ErrorClass: "unauthorized",
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := svc.Activity(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no row was written, so this test proves nothing")
	}

	for _, row := range rows {
		// Every slice field must be non-nil in Go...
		v := reflect.ValueOf(row)
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.Type.Kind() == reflect.Slice && v.Field(i).IsNil() {
				t.Errorf("ActivityRow.%s is a nil slice; it will serialise as null and the dashboard maps over it", f.Name)
			}
		}
		// ...and the serialised form must contain no null list either.
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{`"notes":null`, `"candidates":null`} {
			if strings.Contains(string(raw), field) {
				t.Errorf("activity row carries %s", field)
			}
		}
	}
}
