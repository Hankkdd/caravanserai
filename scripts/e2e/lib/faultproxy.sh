# shellcheck shell=bash
# Sourced by the e2e scripts once $test_root exists. Puts the controllable HTTP
# fault proxy in front of cara-server so a scenario can decide when one of the
# agent's calls fails.
#
# The agent reaches the server only through SERVER_URL, so pointing that at the
# proxy injects the fault without changing any product code: cara runs as it
# ships. The script's own curl calls keep going to $server_url directly, so the
# proxy sees the agent's traffic and nothing else - which is what makes the
# event log a statement about the agent.
#
# Usage, after sourcing:
#
#	faultproxy_start "$server_url"          # sets $faultproxy_url, $faultproxy_control
#	... start the agent with SERVER_URL="$faultproxy_url" ...
#	faultproxy_arm '{"id":"ownership", ... ,"hold":true,"times":1}'
#	faultproxy_wait ownership 60s           # blocks until the call is frozen
#	... build the state the scenario needs ...
#	faultproxy_release ownership            # the failure lands here, not before
#
# faultproxy_wait is the whole point: it returns only once the agent's request
# is parked inside the proxy, so the state built next is in place before the
# failure happens. It calls fail() if nothing gets parked in time, because a
# missed injection must not look like a passing test.

# faultproxy_start <upstream_url> builds the proxy, starts it, and waits until
# its control API answers. It sets $faultproxy_url (point the agent here) and
# $faultproxy_control (drive the scenario here).
faultproxy_start() {
	local upstream="$1"
	local addr_file="$test_root/faultproxy.addr"

	go build -o "$test_root/bin/faultproxy" ./test/injection/cmd/faultproxy ||
		fail "could not build the fault proxy"

	"$test_root/bin/faultproxy" \
		-upstream "$upstream" \
		-listen 127.0.0.1:0 \
		-control 127.0.0.1:0 \
		-addr-file "$addr_file" \
		>"$test_root/faultproxy.log" 2>&1 &
	faultproxy_pid=$!

	local waited=0
	while [[ ! -s "$addr_file" ]]; do
		if ! kill -0 "$faultproxy_pid" 2>/dev/null; then
			fail "fault proxy died on startup; see $test_root/faultproxy.log"
		fi
		if ((waited >= 100)); then
			fail "fault proxy never wrote $addr_file"
		fi
		sleep 0.1
		waited=$((waited + 1))
	done

	faultproxy_url="$(sed -n 's/^listen=//p' "$addr_file")"
	faultproxy_control="$(sed -n 's/^control=//p' "$addr_file")"
	[[ -n "$faultproxy_url" && -n "$faultproxy_control" ]] ||
		fail "could not read the fault proxy's addresses from $addr_file"

	# command curl: the control API is not cara-server, so it must not be sent
	# the break-glass token that lib/auth.sh attaches to $server_url requests.
	waited=0
	until command curl -fsS "$faultproxy_control/control/healthz" >/dev/null 2>&1; do
		if ((waited >= 100)); then
			fail "fault proxy control API never became ready"
		fi
		sleep 0.1
		waited=$((waited + 1))
	done

	log "fault proxy listening on $faultproxy_url, control on $faultproxy_control"
}

# faultproxy_stop ends the proxy, releasing anything still parked so a teardown
# mid-injection cannot leave the agent blocked on a socket.
faultproxy_stop() {
	[[ -n "${faultproxy_pid:-}" ]] || return 0
	kill "$faultproxy_pid" 2>/dev/null || true
	wait "$faultproxy_pid" 2>/dev/null || true
	faultproxy_pid=""
}

# faultproxy_arm <json> arms one rule, or an array of them.
faultproxy_arm() {
	command curl -fsS -X POST "$faultproxy_control/control/rules" \
		-H 'Content-Type: application/json' -d "$1" >/dev/null ||
		fail "could not arm the fault rule: $1"
}

# faultproxy_disarm <rule> removes a rule. Requests it already parked stay
# parked, so release them first.
faultproxy_disarm() {
	command curl -fsS -X DELETE "$faultproxy_control/control/rules/$1" >/dev/null ||
		fail "could not disarm rule $1"
}

# faultproxy_wait <rule> [timeout] blocks until the rule parks a request and
# prints the event recorded for it. A timeout means the injection did not
# happen, which fails the scenario rather than passing it quietly.
faultproxy_wait() {
	local rule="$1" timeout="${2:-60s}" out status
	out="$(command curl -sS -o - -w '\n%{http_code}' \
		--max-time 300 "$faultproxy_control/control/rules/$rule/wait?timeout=$timeout")" ||
		fail "the wait call for rule $rule failed"
	status="${out##*$'\n'}"
	out="${out%$'\n'*}"
	if [[ "$status" != "200" ]]; then
		fail "rule $rule parked no request within $timeout (HTTP $status): $out"
	fi
	printf '%s\n' "$out"
}

# faultproxy_release <rule> [json] lets the rule's oldest parked request finish.
# With no json it finishes the way the rule says.
faultproxy_release() {
	local rule="$1" body="${2:-}" out status
	out="$(command curl -sS -o - -w '\n%{http_code}' -X POST \
		-H 'Content-Type: application/json' -d "$body" \
		"$faultproxy_control/control/rules/$rule/release")" ||
		fail "the release call for rule $rule failed"
	status="${out##*$'\n'}"
	if [[ "$status" != "200" ]]; then
		fail "rule $rule had no parked request to release (HTTP $status)"
	fi
}

# faultproxy_events prints the ordered event log, which is what a scenario
# asserts event order on and where the timing numbers come from.
faultproxy_events() {
	command curl -fsS "$faultproxy_control/control/events" ||
		fail "could not read the fault proxy's event log"
}

# faultproxy_event_kinds prints one event kind per line, for order assertions.
faultproxy_event_kinds() {
	faultproxy_events | python3 -c '
import json, sys
for event in json.load(sys.stdin):
    print(event["kind"])
'
}
