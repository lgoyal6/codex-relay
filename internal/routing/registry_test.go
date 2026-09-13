package routing

import (
	"sync"
	"testing"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/policy"
)

func emptyState() *policy.State {
	return &policy.State{Workspaces: map[string]*policy.WorkspaceState{}}
}

// Version is what the dashboard uses to tell one snapshot from another. Two snapshots sharing
// a version would make a stale view look current. Publishes come from quota ingestion, rule
// edits and the poller, so they genuinely overlap.
func TestConcurrentPublishesProduceUniqueIncreasingVersions(t *testing.T) {
	r := NewRegistry(clock.System())
	const writers = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[int64]bool{}

	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got := r.Publish(emptyState())
			mu.Lock()
			defer mu.Unlock()
			if seen[got.Version] {
				t.Errorf("version %d was handed out twice", got.Version)
			}
			seen[got.Version] = true
		}()
	}
	close(start)
	wg.Wait()

	if len(seen) != writers {
		t.Fatalf("%d distinct versions for %d publishes", len(seen), writers)
	}
	if r.Current().Version <= 0 {
		t.Fatalf("published state carries version %d", r.Current().Version)
	}
}

// A subscriber that never reads must not block a publish. The publisher is on the path that
// ingests quota from a live turn, so one abandoned dashboard tab stalling it would stall
// routing for everything.
func TestASlowSubscriberDoesNotBlockPublish(t *testing.T) {
	r := NewRegistry(clock.System())
	_, cancel := r.Subscribe() // never drained
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			r.Publish(emptyState())
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing stalled behind a subscriber that never reads")
	}
}

// Cancel must remove the subscriber, or every dashboard reload leaks one for the life of the
// process and every publish does more work than the last.
func TestCancelRemovesTheSubscriber(t *testing.T) {
	r := NewRegistry(clock.System())
	var cancels []func()
	for i := 0; i < 100; i++ {
		_, c := r.Subscribe()
		cancels = append(cancels, c)
	}
	r.subsMu.Lock()
	live := len(r.subs)
	r.subsMu.Unlock()
	if live != 100 {
		t.Fatalf("%d subscribers registered, want 100", live)
	}
	for _, c := range cancels {
		c()
	}
	r.subsMu.Lock()
	live = len(r.subs)
	r.subsMu.Unlock()
	if live != 0 {
		t.Fatalf("%d subscribers survived cancellation", live)
	}
}

// Cancelling twice happens whenever a handler's defer runs after an explicit cancel. It must
// not close an already-closed channel, which is a panic that takes the process with it.
func TestDoubleCancelIsSafe(t *testing.T) {
	r := NewRegistry(clock.System())
	ch, cancel := r.Subscribe()
	cancel()
	cancel()
	cancel()
	if _, open := <-ch; open {
		t.Fatal("channel should be closed after cancellation")
	}
}

// Subscribe, publish and cancel all run concurrently in the real service: dashboards connect
// and disconnect while turns publish quota. Under -race this is the test that would surface a
// send on a closed channel.
func TestSubscribeCancelAndPublishRaceCleanly(t *testing.T) {
	r := NewRegistry(clock.System())
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					ch, cancel := r.Subscribe()
					select {
					case <-ch:
					default:
					}
					cancel()
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					r.Publish(emptyState())
					_ = r.Current()
				}
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}
