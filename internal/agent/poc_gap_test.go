package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// TestPoC_OwnershipUnknownMustNotTouchContainers states the safety rule as an
// assertion: when the agent cannot find out which Projects it still owns, it
// must not begin destructive cleanup. Not "must wait the grace period before
// deleting" - must not even stop a container, because an unanswered question
// is not evidence that ownership moved.
//
// loop.go earns this by returning early on the error. The existing suite
// checks that the deletion grace is reset on that path, but never that the
// stop is withheld, so dropping the early return leaves every test green.
func TestPoC_OwnershipUnknownMustNotTouchContainers(t *testing.T) {
	project := identity("default", "poc-target")
	runtime, record := newSweepFixture(project)
	clock := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(clock)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/projects", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the server cannot answer right now", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewClient(zap.NewNop(), server.URL, "test-node")
	reconcileProjects(context.Background(), client, runtime, nil, nil, tracker, nil, nil, zap.NewNop())

	assert.Empty(t, record.stopped, "ownership unknown: no container may be stopped")
	assert.Empty(t, record.removed, "ownership unknown: no container may be removed")
}
