package agent

import (
	"context"
	"errors"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// TestPoC_ResourcesRemovedIsReportedEvenWhenNothingIsLeftToRemove covers the
// poll after a failed report. The first pass removes the resources but cannot
// tell the server; the next pass finds nothing to remove and must report
// anyway, or the obligation stays owed and the deletion never finishes.
//
// An agent killed between removing and reporting lands in the same state, so
// this is also the crash case: cara keeps nothing about cleanup in agent
// memory, and a restart is one more poll. The existing suite never runs a
// task whose resources are already gone, so a mutant that returns early when
// nothing remains (test/injection/mutations/c1-...) leaves it green.
func TestPoC_ResourcesRemovedIsReportedEvenWhenNothingIsLeftToRemove(t *testing.T) {
	server, res := newCleanupCase()
	cleaner := newAssignmentCleaner(res)

	server.reportErr = errors.New("server unreachable")
	runCleanupTasks(context.Background(), server, cleaner, backup.NewCoordinator(), zap.NewNop())
	assert.Empty(t, res.held, "the first pass removes the resources")

	server.reportErr = nil
	server.reported = nil
	runCleanupTasks(context.Background(), server, cleaner, backup.NewCoordinator(), zap.NewNop())
	assert.Equal(t, []v1.AssignmentIdentity{owedAssignment}, server.reported,
		"the pass that found nothing left never reported ResourcesRemoved")
}
