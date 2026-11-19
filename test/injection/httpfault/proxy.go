// Package httpfault is a controllable reverse proxy for injecting faults into
// cara's agent-to-server HTTP calls at a chosen instant.
//
// It exists because the agent reaches the server only through SERVER_URL, so a
// proxy in front of the server can fail, delay or freeze a single call without
// changing any product code. That keeps the injected fault outside the system
// under test: cara runs exactly as it ships.
//
// The point of the proxy is not that it can return an error - a stopped server
// does that too - but that a test can decide WHEN the error lands. A rule with
// Hold set parks the matching request inside the proxy and reports that it is
// parked; the test then builds whatever state the scenario needs and only then
// releases the request as a failure. Both the system and the test are therefore
// at a known point when the fault happens, which is what makes a timing
// scenario reproducible rather than racy.
//
// Every decision is recorded in an ordered event log with timestamps, so a test
// can assert the order in which things happened and can tell an injected
// failure apart from a client that gave up on its own. The latter matters: the
// agent's HTTP client has its own timeout (10s at the time of writing), so a
// request held for longer fails without the proxy ever responding. That is a
// different event, and the log says so.
package httpfault

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Respond says how a held request leaves the proxy.
type Respond string

const (
	// RespondForward sends the request upstream as if nothing had happened.
	RespondForward Respond = "forward"
	// RespondStatus answers the request from the proxy with Status, so the
	// request never reaches the server.
	RespondStatus Respond = "status"
)

// Event kinds recorded in the log.
const (
	// KindForwarded is a request no rule matched.
	KindForwarded = "forwarded"
	// KindMatched is a request a rule matched and acted on at once.
	KindMatched = "matched"
	// KindHeld is a request parked inside the proxy, waiting for a release.
	KindHeld = "held"
	// KindReleased is a held request let go by the control API.
	KindReleased = "released"
	// KindClientGone is a held request whose caller gave up before the test
	// released it. The proxy never answered, so the failure the system saw
	// came from its own timeout, not from this injection.
	KindClientGone = "client_gone"
)

// Rule decides which requests to act on, and how.
//
// An empty Method, PathPrefix, QueryContains and QueryAbsent match every
// request, so a rule should always name enough to be unambiguous. The agent
// asks two different questions on the same path, for example:
//
//	GET /api/v1/projects?phase=Scheduled&nodeRef=node-a   (what should I start)
//	GET /api/v1/projects?nodeRef=node-a                   (what do I still own)
//
// Only the second drives destructive cleanup. Matching it means requiring
// nodeRef and forbidding phase:
//
//	Rule{Method: "GET", PathPrefix: "/api/v1/projects",
//	     QueryContains: []string{"nodeRef=node-a"},
//	     QueryAbsent:   []string{"phase="}}
type Rule struct {
	// ID names the rule in control calls and in the event log.
	ID string `json:"id"`
	// Method, when set, must equal the request method.
	Method string `json:"method,omitempty"`
	// PathPrefix, when set, must prefix the request path.
	PathPrefix string `json:"path_prefix,omitempty"`
	// QueryContains lists substrings the raw query must all contain.
	QueryContains []string `json:"query_contains,omitempty"`
	// QueryAbsent lists substrings the raw query must not contain.
	QueryAbsent []string `json:"query_absent,omitempty"`

	// Hold parks the request until the control API releases it. Use it when
	// the test must reach a known state before the fault lands.
	Hold bool `json:"hold,omitempty"`
	// Respond is what to do with a matched request that is not held, or the
	// default for one that is. RespondStatus needs Status.
	Respond Respond `json:"respond,omitempty"`
	// Status is the code RespondStatus answers with.
	Status int `json:"status,omitempty"`
	// Delay waits this long before acting. On a held request the wait runs
	// after the release.
	Delay Duration `json:"delay,omitempty"`
	// Times limits how many requests the rule acts on. Zero means every one.
	Times int `json:"times,omitempty"`
}

// Release tells a held request how to finish.
type Release struct {
	Respond Respond `json:"respond"`
	Status  int     `json:"status,omitempty"`
	Body    string  `json:"body,omitempty"`
}

// Event is one entry in the proxy's ordered log.
type Event struct {
	Seq        int       `json:"seq"`
	At         time.Time `json:"at"`
	SinceStart Duration  `json:"since_start"`
	Kind       string    `json:"kind"`
	RuleID     string    `json:"rule_id,omitempty"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Query      string    `json:"query,omitempty"`
	Status     int       `json:"status,omitempty"`
	HeldFor    Duration  `json:"held_for,omitempty"`
}

// Duration is a time.Duration that marshals as a string such as "1.5s", so
// rules stay readable in shell scripts and in the event log.
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler. It accepts a duration string and,
// for convenience, a number of nanoseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("parse duration %q: %w", s, err)
		}
		*d = Duration(parsed)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string or nanoseconds: %s", b)
	}
	*d = Duration(n)
	return nil
}

type heldRequest struct {
	event   Event
	release chan releaseOrder
}

// releaseOrder is a Release plus whether to log it. Reset frees stranded
// requests silently: a teardown is not part of the scenario the log describes,
// and an event recorded after Reset cleared the log would be read as belonging
// to the next phase.
type releaseOrder struct {
	how    Release
	silent bool
}

type ruleState struct {
	rule Rule
	// acted counts requests the rule has acted on, for Rule.Times.
	acted int
	// arrivals carries requests parked by this rule, oldest first, so a test
	// that arms a rule before the request arrives never misses it.
	arrivals chan *heldRequest
	// parked holds requests a Wait has taken off arrivals and that a Release
	// has not finished yet.
	parked []*heldRequest
}

// Proxy forwards requests to an upstream server, applying rules on the way.
//
// A Proxy is safe for concurrent use. Its zero value is not usable; call New.
type Proxy struct {
	upstream *url.URL
	reverse  *httputil.ReverseProxy
	start    time.Time
	now      func() time.Time

	mu     sync.Mutex
	rules  []*ruleState
	events []Event
	seq    int
}

// New returns a Proxy forwarding to upstream.
func New(upstream *url.URL) *Proxy {
	p := &Proxy{
		upstream: upstream,
		reverse:  httputil.NewSingleHostReverseProxy(upstream),
		now:      time.Now,
	}
	p.start = p.now()
	return p
}

// SetClock replaces the proxy's wall clock. Tests use it to make event
// timestamps deterministic; it does not affect Delay, which really waits.
func (p *Proxy) SetClock(now func() time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.now = now
	p.start = now()
}

// Upstream returns the address requests are forwarded to.
func (p *Proxy) Upstream() string { return p.upstream.String() }

// AddRule arms a rule. A rule with an ID already in use replaces it, which
// lets a scenario re-arm the same name between phases.
func (p *Proxy) AddRule(r Rule) error {
	if r.ID == "" {
		return fmt.Errorf("rule needs an id")
	}
	if r.Respond == RespondStatus && r.Status == 0 {
		return fmt.Errorf("rule %q responds with a status but names none", r.ID)
	}
	if r.Respond != "" && r.Respond != RespondForward && r.Respond != RespondStatus {
		return fmt.Errorf("rule %q has unknown respond %q", r.ID, r.Respond)
	}
	if r.Times < 0 {
		return fmt.Errorf("rule %q has negative times", r.ID)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	state := &ruleState{rule: r, arrivals: make(chan *heldRequest, 16)}
	for i, existing := range p.rules {
		if existing.rule.ID == r.ID {
			p.rules[i] = state
			return nil
		}
	}
	p.rules = append(p.rules, state)
	return nil
}

// RemoveRule disarms a rule. Requests it already parked stay parked, so a test
// that removes a rule must still release them.
func (p *Proxy) RemoveRule(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, state := range p.rules {
		if state.rule.ID == id {
			p.rules = append(p.rules[:i], p.rules[i+1:]...)
			return
		}
	}
}

// Events returns a copy of the log, oldest first.
func (p *Proxy) Events() []Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Event, len(p.events))
	copy(out, p.events)
	return out
}

// Reset disarms every rule and empties the log. Requests already parked are
// released as failures first, so Reset cannot leave a caller hanging.
func (p *Proxy) Reset() {
	p.mu.Lock()
	stranded := make([]*heldRequest, 0)
	for _, state := range p.rules {
		stranded = append(stranded, state.parked...)
		state.parked = nil
		for {
			select {
			case held := <-state.arrivals:
				stranded = append(stranded, held)
				continue
			default:
			}
			break
		}
	}
	p.rules = nil
	p.events = nil
	p.seq = 0
	p.start = p.now()
	p.mu.Unlock()

	for _, held := range stranded {
		select {
		case held.release <- releaseOrder{
			how:    Release{Respond: RespondStatus, Status: http.StatusServiceUnavailable},
			silent: true,
		}:
		default:
		}
	}
}

// Wait blocks until the rule parks a request, and returns the event recorded
// for it. It reports false if the rule parks nothing within timeout, which a
// scenario should treat as a failed injection rather than a passing test.
//
// Wait takes the request off the arrival queue; Release then finishes it.
func (p *Proxy) Wait(id string, timeout time.Duration) (Event, bool) {
	p.mu.Lock()
	var arrivals chan *heldRequest
	for _, state := range p.rules {
		if state.rule.ID == id {
			arrivals = state.arrivals
			break
		}
	}
	p.mu.Unlock()
	if arrivals == nil {
		return Event{}, false
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case held := <-arrivals:
		p.mu.Lock()
		for _, state := range p.rules {
			if state.rule.ID == id {
				state.parked = append(state.parked, held)
				break
			}
		}
		p.mu.Unlock()
		return held.event, true
	case <-timer.C:
		return Event{}, false
	}
}

// Release finishes the rule's oldest parked request. It reports false when the
// rule has nothing parked, so a test can tell a real release from a no-op.
func (p *Proxy) Release(id string, how Release) bool {
	p.mu.Lock()
	var held *heldRequest
	for _, state := range p.rules {
		if state.rule.ID != id {
			continue
		}
		if len(state.parked) == 0 {
			break
		}
		held, state.parked = state.parked[0], state.parked[1:]
		break
	}
	p.mu.Unlock()
	if held == nil {
		return false
	}

	select {
	case held.release <- releaseOrder{how: how}:
		return true
	case <-time.After(5 * time.Second):
		// The caller vanished while we held it; its client_gone event is
		// already in the log.
		return false
	}
}

// Handler is the proxy itself: point SERVER_URL at it.
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(p.serve)
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	state, rule := p.claim(r)
	if state == nil {
		p.record(Event{Kind: KindForwarded, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery})
		p.reverse.ServeHTTP(w, r)
		return
	}

	if !rule.Hold {
		p.record(Event{
			Kind: KindMatched, RuleID: rule.ID,
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		})
		p.finish(w, r, rule.ID, Release{Respond: rule.Respond, Status: rule.Status}, time.Duration(rule.Delay))
		return
	}

	held := &heldRequest{
		event: p.record(Event{
			Kind: KindHeld, RuleID: rule.ID,
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		}),
		release: make(chan releaseOrder, 1),
	}
	heldAt := p.now()

	select {
	case state.arrivals <- held:
	default:
		// More parked requests than the queue can take. Failing loudly beats
		// quietly forwarding a request the scenario believes is frozen.
		p.record(Event{
			Kind: KindMatched, RuleID: rule.ID, Status: http.StatusInsufficientStorage,
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		})
		http.Error(w, "httpfault: hold queue full", http.StatusInsufficientStorage)
		return
	}

	select {
	case order := <-held.release:
		how := order.how
		if how.Respond == "" {
			how.Respond = rule.Respond
			how.Status = rule.Status
		}
		if order.silent {
			// Answer so the caller is not left hanging, but log nothing and
			// skip the rule's delay: the scenario is already over.
			respondWithStatus(w, rule.ID, how)
			return
		}
		p.record(Event{
			Kind: KindReleased, RuleID: rule.ID,
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			HeldFor: Duration(p.now().Sub(heldAt)),
		})
		p.finish(w, r, rule.ID, how, time.Duration(rule.Delay))
	case <-r.Context().Done():
		// The client's own timeout fired first. Record it: the system under
		// test saw a failure this injection did not cause.
		p.record(Event{
			Kind: KindClientGone, RuleID: rule.ID,
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			HeldFor: Duration(p.now().Sub(heldAt)),
		})
	}
}

// claim finds the first rule matching r and counts the match, so that
// Rule.Times is enforced exactly once per request even under concurrency.
func (p *Proxy) claim(r *http.Request) (*ruleState, Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, state := range p.rules {
		if !matches(state.rule, r) {
			continue
		}
		if state.rule.Times > 0 && state.acted >= state.rule.Times {
			continue
		}
		state.acted++
		return state, state.rule
	}
	return nil, Rule{}
}

func (p *Proxy) finish(w http.ResponseWriter, r *http.Request, ruleID string, how Release, delay time.Duration) {
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			p.record(Event{
				Kind: KindClientGone, RuleID: ruleID,
				Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			})
			return
		}
	}

	if how.Respond == RespondStatus {
		respondWithStatus(w, ruleID, how)
		return
	}
	p.reverse.ServeHTTP(w, r)
}

func respondWithStatus(w http.ResponseWriter, ruleID string, how Release) {
	status := how.Status
	if status == 0 {
		status = http.StatusServiceUnavailable
	}
	body := how.Body
	if body == "" {
		body = fmt.Sprintf("httpfault: injected %d for rule %q\n", status, ruleID)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (p *Proxy) record(e Event) Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	e.Seq = p.seq
	e.At = p.now()
	e.SinceStart = Duration(e.At.Sub(p.start))
	p.events = append(p.events, e)
	return e
}

func matches(r Rule, req *http.Request) bool {
	if r.Method != "" && !strings.EqualFold(r.Method, req.Method) {
		return false
	}
	if r.PathPrefix != "" && !strings.HasPrefix(req.URL.Path, r.PathPrefix) {
		return false
	}
	query := req.URL.RawQuery
	for _, want := range r.QueryContains {
		if !strings.Contains(query, want) {
			return false
		}
	}
	for _, unwanted := range r.QueryAbsent {
		if strings.Contains(query, unwanted) {
			return false
		}
	}
	return true
}
