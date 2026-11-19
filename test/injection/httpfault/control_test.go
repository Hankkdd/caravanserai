package httpfault_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"NYCU-SDC/caravanserai/test/injection/httpfault"
)

// The E2E scripts drive the proxy with curl, so the HTTP surface is what they
// actually depend on. These tests exercise it the same way a script does.
func newControlled(t *testing.T) (front, control string, reached *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream"))
	}))
	t.Cleanup(upstream.Close)

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	proxy := httpfault.New(target)

	frontServer := httptest.NewServer(proxy.Handler())
	controlServer := httptest.NewServer(proxy.ControlHandler())
	t.Cleanup(func() {
		proxy.Reset()
		frontServer.Close()
		controlServer.Close()
	})
	return frontServer.URL, controlServer.URL, &hits
}

func post(t *testing.T, target, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(target, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s: %v", target, err)
	}
	return resp.StatusCode, string(out)
}

const ownershipRuleJSON = `{
  "id": "ownership",
  "method": "GET",
  "path_prefix": "/api/v1/projects",
  "query_contains": ["nodeRef=node-a"],
  "query_absent": ["phase="],
  "hold": true,
  "respond": "status",
  "status": 503,
  "times": 1
}`

// The whole handshake a scenario performs, over HTTP: arm, wait for the parked
// request, build state, release as a failure, then read the log back.
func TestControl_ArmWaitReleaseOverHTTP(t *testing.T) {
	front, control, reached := newControlled(t)

	if status, body := post(t, control+"/control/rules", ownershipRuleJSON); status != http.StatusOK {
		t.Fatalf("arming the rule gave %d: %s", status, body)
	}

	requestDone := make(chan int, 1)
	go func() {
		resp, err := http.Get(front + ownershipQuery)
		if err != nil {
			requestDone <- -1
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		requestDone <- resp.StatusCode
	}()

	waitDone := make(chan httpfault.Event, 1)
	go func() {
		resp, err := http.Get(control + "/control/rules/ownership/wait?timeout=5s")
		if err != nil {
			waitDone <- httpfault.Event{}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			waitDone <- httpfault.Event{}
			return
		}
		var event httpfault.Event
		if err := json.NewDecoder(resp.Body).Decode(&event); err != nil {
			waitDone <- httpfault.Event{}
			return
		}
		waitDone <- event
	}()

	var parked httpfault.Event
	select {
	case parked = <-waitDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the wait call never returned")
	}
	if parked.Kind != httpfault.KindHeld {
		t.Fatalf("wait returned kind %q, want %q", parked.Kind, httpfault.KindHeld)
	}
	if parked.Query != "nodeRef=node-a" {
		t.Errorf("wait returned query %q, want the ownership query", parked.Query)
	}

	// The request must still be frozen while the scenario works.
	select {
	case status := <-requestDone:
		t.Fatalf("request finished with %d while it should have been parked", status)
	case <-time.After(50 * time.Millisecond):
	}

	if status, body := post(t, control+"/control/rules/ownership/release", ""); status != http.StatusOK {
		t.Fatalf("release gave %d: %s", status, body)
	}
	select {
	case status := <-requestDone:
		if status != http.StatusServiceUnavailable {
			t.Errorf("request status = %d, want the injected 503", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request never finished after the release")
	}
	if reached.Load() != 0 {
		t.Errorf("upstream saw %d requests, want 0", reached.Load())
	}

	resp, err := http.Get(control + "/control/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var events []httpfault.Event
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	wantKinds(t, events, httpfault.KindHeld, httpfault.KindReleased)
}

// A script that cannot see a failed injection would report a pass for a fault
// it never delivered, so these two answers must be distinguishable by status.
func TestControl_ReportsFailedInjection(t *testing.T) {
	_, control, _ := newControlled(t)

	if status, body := post(t, control+"/control/rules", ownershipRuleJSON); status != http.StatusOK {
		t.Fatalf("arming the rule gave %d: %s", status, body)
	}

	resp, err := http.Get(control + "/control/rules/ownership/wait?timeout=50ms")
	if err != nil {
		t.Fatalf("GET wait: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Errorf("wait with nothing parked gave %d, want 408", resp.StatusCode)
	}

	if status, _ := post(t, control+"/control/rules/ownership/release", ""); status != http.StatusConflict {
		t.Errorf("release with nothing parked gave %d, want 409", status)
	}
}

func TestControl_RejectsBadInput(t *testing.T) {
	_, control, _ := newControlled(t)

	cases := map[string]struct {
		target string
		body   string
		want   int
	}{
		"rule without an id":    {control + "/control/rules", `{"respond":"forward"}`, http.StatusBadRequest},
		"status without a code": {control + "/control/rules", `{"id":"a","respond":"status"}`, http.StatusBadRequest},
		"unknown respond":       {control + "/control/rules", `{"id":"a","respond":"explode"}`, http.StatusBadRequest},
		"empty array":           {control + "/control/rules", `[]`, http.StatusBadRequest},
		"not json":              {control + "/control/rules", `nonsense`, http.StatusBadRequest},
	}
	for name, tc := range cases {
		if status, body := post(t, tc.target, tc.body); status != tc.want {
			t.Errorf("%s gave %d, want %d: %s", name, status, tc.want, body)
		}
	}

	resp, err := http.Get(control + "/control/rules/ownership/wait?timeout=banana")
	if err != nil {
		t.Fatalf("GET wait: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unparseable timeout gave %d, want 400", resp.StatusCode)
	}
}

// Arming a whole scenario in one call keeps the shell side short.
func TestControl_ArmsSeveralRulesAtOnce(t *testing.T) {
	front, control, _ := newControlled(t)

	body := `[
      {"id":"heartbeat","method":"POST","path_prefix":"/api/v1/nodes","respond":"status","status":500},
      {"id":"ownership","method":"GET","path_prefix":"/api/v1/projects",
       "query_contains":["nodeRef=node-a"],"query_absent":["phase="],
       "respond":"status","status":503}
    ]`
	if status, out := post(t, control+"/control/rules", body); status != http.StatusOK {
		t.Fatalf("arming gave %d: %s", status, out)
	}

	// The agent's heartbeat is a POST, which is what the rule requires.
	if status, _ := post(t, front+"/api/v1/nodes/node-a/heartbeat", "{}"); status != http.StatusInternalServerError {
		t.Errorf("heartbeat status = %d, want 500", status)
	}
	if status, _ := get(t, front, ownershipQuery); status != http.StatusServiceUnavailable {
		t.Errorf("ownership status = %d, want 503", status)
	}
	if status, _ := get(t, front, scheduledQuery); status != http.StatusOK {
		t.Errorf("Scheduled status = %d, want 200: neither rule should match it", status)
	}
}

func TestControl_DeleteAndReset(t *testing.T) {
	front, control, _ := newControlled(t)

	if status, _ := post(t, control+"/control/rules",
		`{"id":"ownership","method":"GET","path_prefix":"/api/v1/projects","respond":"status","status":503}`,
	); status != http.StatusOK {
		t.Fatal("arming the rule failed")
	}
	if status, _ := get(t, front, ownershipQuery); status != http.StatusServiceUnavailable {
		t.Fatalf("rule did not take effect")
	}

	req, err := http.NewRequest(http.MethodDelete, control+"/control/rules/ownership", nil)
	if err != nil {
		t.Fatalf("build DELETE: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE rule: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE gave %d, want 204", resp.StatusCode)
	}
	if status, _ := get(t, front, ownershipQuery); status != http.StatusOK {
		t.Errorf("status = %d after DELETE, want 200", status)
	}

	if status, _ := post(t, control+"/control/reset", ""); status != http.StatusNoContent {
		t.Errorf("reset gave %d, want 204", status)
	}
	resp2, err := http.Get(control + "/control/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	var events []httpfault.Event
	if err := json.NewDecoder(resp2.Body).Decode(&events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("reset left %d events behind", len(events))
	}
}

// A script waits for this before it starts the system under test.
func TestControl_HealthzNamesTheUpstream(t *testing.T) {
	_, control, _ := newControlled(t)

	resp, err := http.Get(control + "/control/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz gave %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	if body["upstream"] == "" {
		t.Error("healthz did not name the upstream")
	}
}
