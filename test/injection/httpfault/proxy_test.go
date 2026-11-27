package httpfault_test

import (
	"context"
	"encoding/json"
	"errors"
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

// The two query shapes the agent really sends. They share a path, and only the
// second one drives destructive cleanup, so a rule that cannot tell them apart
// would inject into the wrong call and still look like it worked.
const (
	scheduledQuery = "/api/v1/projects?phase=Scheduled&nodeRef=node-a"
	ownershipQuery = "/api/v1/projects?nodeRef=node-a"
)

// ownershipRule matches "what do I still own" and not "what should I start".
func ownershipRule(id string) httpfault.Rule {
	return httpfault.Rule{
		ID:            id,
		Method:        http.MethodGet,
		PathPrefix:    "/api/v1/projects",
		QueryContains: []string{"nodeRef=node-a"},
		QueryAbsent:   []string{"phase="},
	}
}

// newProxy starts a proxy in front of a counting upstream and returns both the
// proxy and the number of requests that reached the upstream so far.
func newProxy(t *testing.T) (*httpfault.Proxy, *httptest.Server, *atomic.Int64) {
	t.Helper()

	var reached atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream:" + r.URL.RequestURI()))
	}))
	t.Cleanup(upstream.Close)

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	proxy := httpfault.New(target)

	front := httptest.NewServer(proxy.Handler())
	t.Cleanup(func() {
		// Release anything still parked first, or Close blocks on it.
		proxy.Reset()
		front.Close()
	})
	return proxy, front, &reached
}

func get(t *testing.T, base, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s: %v", path, err)
	}
	return resp.StatusCode, string(body)
}

// kinds lists the event kinds in order, which is what a scenario asserts on.
func kinds(events []httpfault.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func wantKinds(t *testing.T, events []httpfault.Event, want ...string) {
	t.Helper()
	got := kinds(events)
	if len(got) != len(want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event kinds = %v, want %v", got, want)
		}
	}
}

func TestProxy_ForwardsWhenNoRuleMatches(t *testing.T) {
	proxy, front, reached := newProxy(t)

	status, body := get(t, front.URL, ownershipQuery)
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if !strings.HasPrefix(body, "upstream:") {
		t.Errorf("body = %q, want the upstream's answer", body)
	}
	if reached.Load() != 1 {
		t.Errorf("upstream saw %d requests, want 1", reached.Load())
	}
	wantKinds(t, proxy.Events(), httpfault.KindForwarded)
}

// A rule that cannot separate the two project queries is the failure mode most
// likely to go unnoticed, because injecting into the Scheduled query also
// produces a log line and a failed request.
func TestProxy_OwnershipRuleIgnoresScheduledQuery(t *testing.T) {
	proxy, front, reached := newProxy(t)

	rule := ownershipRule("ownership")
	rule.Respond = httpfault.RespondStatus
	rule.Status = http.StatusServiceUnavailable
	if err := proxy.AddRule(rule); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	if status, _ := get(t, front.URL, scheduledQuery); status != http.StatusOK {
		t.Errorf("Scheduled query status = %d, want 200: the rule must not match it", status)
	}
	if status, _ := get(t, front.URL, ownershipQuery); status != http.StatusServiceUnavailable {
		t.Errorf("ownership query status = %d, want 503", status)
	}
	if reached.Load() != 1 {
		t.Errorf("upstream saw %d requests, want only the Scheduled one", reached.Load())
	}
	wantKinds(t, proxy.Events(), httpfault.KindForwarded, httpfault.KindMatched)
}

// The synchronous handshake: the scenario learns the request is parked, builds
// its state, and only then lets the failure land. Both sides are at a known
// point, which is what makes the timing reproducible.
func TestProxy_HoldLetsTheTestActBeforeTheFailureLands(t *testing.T) {
	proxy, front, reached := newProxy(t)

	rule := ownershipRule("ownership")
	rule.Hold = true
	rule.Times = 1
	if err := proxy.AddRule(rule); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Get(front.URL + ownershipQuery)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		done <- result{status: resp.StatusCode}
	}()

	event, ok := proxy.Wait("ownership", 5*time.Second)
	if !ok {
		t.Fatal("Wait reported no parked request; the injection did not happen")
	}
	if event.Kind != httpfault.KindHeld {
		t.Errorf("parked event kind = %q, want %q", event.Kind, httpfault.KindHeld)
	}
	if event.Query != "nodeRef=node-a" {
		t.Errorf("parked event query = %q, want the ownership query", event.Query)
	}

	// Stand-in for the state a scenario builds while the request is frozen,
	// such as moving the Project's ownership to another node.
	setupRan := false
	select {
	case r := <-done:
		t.Fatalf("request finished while it should have been parked: %+v", r)
	case <-time.After(50 * time.Millisecond):
		setupRan = true
	}
	if !setupRan {
		t.Fatal("setup window never opened")
	}

	if !proxy.Release("ownership", httpfault.Release{
		Respond: httpfault.RespondStatus,
		Status:  http.StatusServiceUnavailable,
	}) {
		t.Fatal("Release reported nothing parked")
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("request failed at the transport: %v", r.err)
		}
		if r.status != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", r.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request never finished after Release")
	}

	if reached.Load() != 0 {
		t.Errorf("upstream saw %d requests, want 0: the failure must be injected, not forwarded", reached.Load())
	}
	wantKinds(t, proxy.Events(), httpfault.KindHeld, httpfault.KindReleased)
}

// The request may arrive before the scenario gets around to waiting for it.
// The agent polls on its own schedule, so this ordering is the common one.
func TestProxy_HoldSurvivesRequestArrivingBeforeWait(t *testing.T) {
	proxy, front, _ := newProxy(t)

	rule := ownershipRule("ownership")
	rule.Hold = true
	if err := proxy.AddRule(rule); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	done := make(chan int, 1)
	go func() {
		resp, err := http.Get(front.URL + ownershipQuery)
		if err != nil {
			done <- -1
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		done <- resp.StatusCode
	}()

	// Let the request get parked before anybody waits on it.
	deadline := time.Now().Add(5 * time.Second)
	for len(proxy.Events()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if _, ok := proxy.Wait("ownership", 5*time.Second); !ok {
		t.Fatal("Wait missed a request that was already parked")
	}
	if !proxy.Release("ownership", httpfault.Release{Respond: httpfault.RespondForward}) {
		t.Fatal("Release reported nothing parked")
	}
	select {
	case status := <-done:
		if status != http.StatusOK {
			t.Errorf("status = %d, want 200 after a forwarding release", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request never finished after Release")
	}
}

// A release can also forward, which a scenario uses to prove the system
// behaves correctly once the fault clears.
func TestProxy_ReleaseCanForwardUpstream(t *testing.T) {
	proxy, front, reached := newProxy(t)

	rule := ownershipRule("ownership")
	rule.Hold = true
	if err := proxy.AddRule(rule); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	done := make(chan string, 1)
	go func() {
		resp, err := http.Get(front.URL + ownershipQuery)
		if err != nil {
			done <- "error:" + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		done <- string(body)
	}()

	if _, ok := proxy.Wait("ownership", 5*time.Second); !ok {
		t.Fatal("Wait reported no parked request")
	}
	if !proxy.Release("ownership", httpfault.Release{Respond: httpfault.RespondForward}) {
		t.Fatal("Release reported nothing parked")
	}

	select {
	case body := <-done:
		if !strings.HasPrefix(body, "upstream:") {
			t.Errorf("body = %q, want the upstream's answer", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request never finished after Release")
	}
	if reached.Load() != 1 {
		t.Errorf("upstream saw %d requests, want 1", reached.Load())
	}
}

// The agent's HTTP client has its own timeout, so a request held for too long
// fails without the proxy answering. The log must say so, because a scenario
// that cannot tell this apart from an injected failure would report a pass for
// a fault it never delivered.
func TestProxy_RecordsClientGaveUpWhileHeld(t *testing.T) {
	proxy, front, reached := newProxy(t)

	rule := ownershipRule("ownership")
	rule.Hold = true
	if err := proxy.AddRule(rule); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+ownershipQuery, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("request succeeded, want the client's own timeout")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want a deadline exceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request never gave up")
	}

	deadline := time.Now().Add(5 * time.Second)
	var events []httpfault.Event
	for time.Now().Before(deadline) {
		events = proxy.Events()
		if len(events) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	wantKinds(t, events, httpfault.KindHeld, httpfault.KindClientGone)
	if events[1].HeldFor <= 0 {
		t.Errorf("client_gone held_for = %v, want how long it waited", time.Duration(events[1].HeldFor))
	}
	if reached.Load() != 0 {
		t.Errorf("upstream saw %d requests, want 0", reached.Load())
	}
}

func TestProxy_TimesStopsInjectingAfterTheLimit(t *testing.T) {
	proxy, front, reached := newProxy(t)

	rule := ownershipRule("ownership")
	rule.Respond = httpfault.RespondStatus
	rule.Status = http.StatusServiceUnavailable
	rule.Times = 1
	if err := proxy.AddRule(rule); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	if status, _ := get(t, front.URL, ownershipQuery); status != http.StatusServiceUnavailable {
		t.Errorf("first status = %d, want 503", status)
	}
	if status, _ := get(t, front.URL, ownershipQuery); status != http.StatusOK {
		t.Errorf("second status = %d, want 200: the rule was spent", status)
	}
	if reached.Load() != 1 {
		t.Errorf("upstream saw %d requests, want 1", reached.Load())
	}
	wantKinds(t, proxy.Events(), httpfault.KindMatched, httpfault.KindForwarded)
}

// A server that answers 200 with a payload the agent cannot read is a different
// fault from one that answers 500, and the agent takes a different path for it,
// so a rule must be able to answer that way on every request, without holding.
func TestProxy_RuleCanAnswerWithAMalformedBody(t *testing.T) {
	proxy, front, reached := newProxy(t)

	rule := ownershipRule("truncated")
	rule.Respond = httpfault.RespondStatus
	rule.Status = http.StatusOK
	rule.Body = `{"items":[`
	if err := proxy.AddRule(rule); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	for i := 0; i < 2; i++ {
		status, body := get(t, front.URL, ownershipQuery)
		if status != http.StatusOK || body != `{"items":[` {
			t.Errorf("request %d = %d %q, want 200 with the rule's body", i, status, body)
		}
	}
	if reached.Load() != 0 {
		t.Errorf("upstream saw %d requests, want 0", reached.Load())
	}
}

// A scenario must be able to tell "the fault landed" from "nothing happened",
// so both of these report failure rather than succeeding quietly.
func TestProxy_WaitAndReleaseReportWhenNothingHappened(t *testing.T) {
	proxy, _, _ := newProxy(t)

	if err := proxy.AddRule(ownershipRule("ownership")); err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	if _, ok := proxy.Wait("ownership", 50*time.Millisecond); ok {
		t.Error("Wait reported a parked request when none arrived")
	}
	if _, ok := proxy.Wait("no-such-rule", 50*time.Millisecond); ok {
		t.Error("Wait reported a parked request for an unknown rule")
	}
	if proxy.Release("ownership", httpfault.Release{Respond: httpfault.RespondForward}) {
		t.Error("Release reported success with nothing parked")
	}
}

func TestProxy_AddRuleRejectsUnusableRules(t *testing.T) {
	proxy, _, _ := newProxy(t)

	cases := map[string]httpfault.Rule{
		"no id":              {Respond: httpfault.RespondForward},
		"status without one": {ID: "a", Respond: httpfault.RespondStatus},
		"unknown respond":    {ID: "a", Respond: httpfault.Respond("explode")},
		"negative times":     {ID: "a", Times: -1},
	}
	for name, rule := range cases {
		if err := proxy.AddRule(rule); err == nil {
			t.Errorf("AddRule(%s) accepted an unusable rule", name)
		}
	}
}

// Re-arming under the same ID is how a scenario moves between phases, so it
// must replace the rule rather than leave two matching the same request.
func TestProxy_AddRuleReplacesSameID(t *testing.T) {
	proxy, front, _ := newProxy(t)

	first := ownershipRule("ownership")
	first.Respond = httpfault.RespondStatus
	first.Status = http.StatusServiceUnavailable
	if err := proxy.AddRule(first); err != nil {
		t.Fatalf("AddRule first: %v", err)
	}

	second := ownershipRule("ownership")
	second.Respond = httpfault.RespondStatus
	second.Status = http.StatusInternalServerError
	if err := proxy.AddRule(second); err != nil {
		t.Fatalf("AddRule second: %v", err)
	}

	if status, _ := get(t, front.URL, ownershipQuery); status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 from the replacing rule", status)
	}
	wantKinds(t, proxy.Events(), httpfault.KindMatched)
}

func TestProxy_ResetFreesParkedRequests(t *testing.T) {
	proxy, front, _ := newProxy(t)

	rule := ownershipRule("ownership")
	rule.Hold = true
	if err := proxy.AddRule(rule); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	done := make(chan int, 1)
	go func() {
		resp, err := http.Get(front.URL + ownershipQuery)
		if err != nil {
			done <- -1
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		done <- resp.StatusCode
	}()

	if _, ok := proxy.Wait("ownership", 5*time.Second); !ok {
		t.Fatal("Wait reported no parked request")
	}
	proxy.Reset()

	select {
	case status := <-done:
		if status != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 from Reset", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reset left the request parked")
	}
	if len(proxy.Events()) != 0 {
		t.Errorf("Reset left %d events behind", len(proxy.Events()))
	}
}

// The event log is what a scenario asserts order on and what the reproducibility
// numbers come from, so sequence numbers must be gapless and increasing.
func TestProxy_EventLogIsOrdered(t *testing.T) {
	proxy, front, _ := newProxy(t)

	for range 3 {
		if status, _ := get(t, front.URL, ownershipQuery); status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
	}

	events := proxy.Events()
	if len(events) != 3 {
		t.Fatalf("logged %d events, want 3", len(events))
	}
	for i, e := range events {
		if e.Seq != i+1 {
			t.Errorf("event %d has seq %d, want %d", i, e.Seq, i+1)
		}
		if e.At.IsZero() {
			t.Errorf("event %d has no timestamp", i)
		}
		if i > 0 && e.SinceStart < events[i-1].SinceStart {
			t.Errorf("event %d went backwards in time", i)
		}
	}
}

func TestDuration_RoundTripsAsAString(t *testing.T) {
	encoded, err := json.Marshal(httpfault.Duration(1500 * time.Millisecond))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != `"1.5s"` {
		t.Errorf("encoded = %s, want \"1.5s\"", encoded)
	}

	var fromString httpfault.Duration
	if err := json.Unmarshal([]byte(`"2m30s"`), &fromString); err != nil {
		t.Fatalf("unmarshal string: %v", err)
	}
	if time.Duration(fromString) != 150*time.Second {
		t.Errorf("decoded = %v, want 2m30s", time.Duration(fromString))
	}

	var fromNanos httpfault.Duration
	if err := json.Unmarshal([]byte(`5000000000`), &fromNanos); err != nil {
		t.Fatalf("unmarshal nanoseconds: %v", err)
	}
	if time.Duration(fromNanos) != 5*time.Second {
		t.Errorf("decoded = %v, want 5s", time.Duration(fromNanos))
	}

	var bad httpfault.Duration
	if err := json.Unmarshal([]byte(`"not a duration"`), &bad); err == nil {
		t.Error("unmarshal accepted a non-duration string")
	}
}
