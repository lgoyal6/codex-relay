package service

import "testing"

// A first token that genuinely arrived in under a millisecond is a real measurement. Storing
// it as NULL made it indistinguishable from "never measured", and Activity then showed no
// timed row at all against a fast local upstream.
func TestMeasuredZeroIsStoredNotNulled(t *testing.T) {
	if got := nullIfUnmeasured(0); got == nil {
		t.Fatal("a measured 0 ms must be stored as 0, not NULL")
	}
	if got := nullIfUnmeasured(-1); got != nil {
		t.Fatalf("an unmeasured value must be stored as NULL, got %v", got)
	}
	if got := nullIfUnmeasured(42); got != int64(42) {
		t.Fatalf("a measured value must round-trip, got %v", got)
	}
}
