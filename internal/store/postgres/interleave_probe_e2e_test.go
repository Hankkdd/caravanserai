//go:build e2e

package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/store"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These probes ask one question of the store's compare-and-swap writes: when
// another write lands between a writer's read and its swap, does the other
// write survive? Each probe counts a write as lost when the final counter is
// lower than the number of increments that reported success.

func reschedules(t *testing.T, s *pgstore.Store, name string) int {
	t.Helper()
	p, err := s.GetProject(context.Background(), name)
	require.NoError(t, err)
	if p.Status.Recovery == nil {
		return 0
	}
	return int(p.Status.Recovery.RescheduleAttempts)
}

func bumpReschedules(st *v1.ProjectStatus) error {
	if st.Recovery == nil {
		st.Recovery = &v1.ProjectRecoveryStatus{}
	}
	st.Recovery.RescheduleAttempts++
	return nil
}

// Ordered: the hook places a controller write exactly between the read and the
// swap of a ResourcesRemoved report, once.
func TestProbe_OrderedWriteBetweenReadAndSwapSurvives(t *testing.T) {
	s, _ := newE2EStore(t)
	ctx := context.Background()
	const name = "ordered"

	p := createRunning(t, s, name, "node-b", 4)
	owner := store.AssignmentRef{UID: p.ObjectMeta.UID, NodeRef: "node-a", Generation: 3}
	owe(t, s, name, owner)
	before := reschedules(t, s, name)

	fired := false
	hook := func() {
		if fired {
			return
		}
		fired = true
		require.NoError(t, s.UpdateProjectStatusWithRetry(ctx, name, bumpReschedules))
	}
	require.NoError(t, pgstore.RecordResourcesRemovedWithHook(s, ctx, name, owner, time.Now(), hook))

	after, err := s.GetProject(ctx, name)
	require.NoError(t, err)
	assert.Equal(t, before+1, int(after.Status.Recovery.RescheduleAttempts),
		"the controller write that landed between read and swap was overwritten")
	require.Len(t, after.Status.Recovery.CleanupObligations, 1)
	assert.NotNil(t, after.Status.Recovery.CleanupObligations[0].ResourcesRemoved)
}

// Unordered: start two writes at the same instant, many times, and count how
// often one of them is lost. No control over who reads or swaps first.
func racePairs(t *testing.T, s *pgstore.Store, name string, trials int, first func() error) (lost int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < trials; i++ {
		before := reschedules(t, s, name)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var errA, errB error
		wg.Add(2)
		go func() { defer wg.Done(); <-start; errA = first() }()
		go func() {
			defer wg.Done()
			<-start
			errB = s.UpdateProjectStatusWithRetry(ctx, name, bumpReschedules)
		}()
		close(start)
		wg.Wait()
		want := before
		if errA == nil {
			want++
		}
		if errB == nil {
			want++
		}
		if reschedules(t, s, name) < want {
			lost++
		}
	}
	return lost
}

const raceTrials = 300

func TestProbe_UnorderedUIDPinnedWritesKeepBoth(t *testing.T) {
	s, _ := newE2EStore(t)
	ctx := context.Background()
	p := createRunning(t, s, "uid-race", "node-b", 4)
	lost := racePairs(t, s, "uid-race", raceTrials, func() error {
		return s.UpdateProjectStatusForUID(ctx, "uid-race", p.ObjectMeta.UID, bumpReschedules)
	})
	t.Logf("UID-pinned writes: %d of %d simultaneous pairs lost a write", lost, raceTrials)
	assert.Zero(t, lost)
}

func TestProbe_UnorderedFencedReportsKeepBoth(t *testing.T) {
	s, _ := newE2EStore(t)
	ctx := context.Background()
	p := createRunning(t, s, "fenced-race", "node-b", 4)
	ref := store.AssignmentRef{UID: p.ObjectMeta.UID, NodeRef: "node-b", Generation: 4}
	lost := racePairs(t, s, "fenced-race", raceTrials, func() error {
		return s.ReportProjectStatusFenced(ctx, "fenced-race", ref, bumpReschedules)
	})
	t.Logf("fenced agent reports: %d of %d simultaneous pairs lost a write", lost, raceTrials)
	assert.Zero(t, lost)
}
