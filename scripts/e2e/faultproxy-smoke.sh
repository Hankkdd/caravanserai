#!/usr/bin/env bash
# Checks the fault proxy the way the E2E scenarios use it: as a binary, driven
# over HTTP from a shell script.
#
# The Go tests cover the proxy's behaviour. This covers the part they cannot:
# that the built binary starts, announces its addresses, and answers the control
# calls scripts/e2e/lib/faultproxy.sh makes. It stands in for cara-server with a
# few lines of Python, so it needs no Docker and no database and finishes in
# seconds - cheap enough to run on every push, unlike the Docker-in-Docker
# rehearsals.
#
# Usage: scripts/e2e/faultproxy-smoke.sh   (from the repository root)

set -euo pipefail

log() { printf '[faultproxy-smoke] %s\n' "$*"; }
fail() {
	log "FAIL: $*"
	exit 1
}

test_root="$(mktemp -d "${TMPDIR:-/tmp}/cara-faultproxy-smoke.XXXXXX")"
mkdir -p "$test_root/bin"

cleanup() {
	faultproxy_stop 2>/dev/null || true
	[[ -n "${upstream_pid:-}" ]] && kill "$upstream_pid" 2>/dev/null
	wait 2>/dev/null || true
	case "$(basename "$test_root")" in
	cara-faultproxy-smoke.*) rm -rf -- "$test_root" ;;
	esac
}
trap cleanup EXIT

# A stand-in for cara-server: answers everything, and counts what reaches it so
# the test can prove an injected failure never got forwarded.
cat >"$test_root/upstream.py" <<'PY'
import http.server, json, sys, threading

reached = []
lock = threading.Lock()


class Handler(http.server.BaseHTTPRequestHandler):
    def respond(self):
        with lock:
            if self.path == "/__count":
                body = json.dumps({"reached": len(reached)}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            reached.append(self.path)
        body = b"upstream"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_GET = respond
    do_POST = respond

    def log_message(self, *args):
        pass


server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
print(f"http://127.0.0.1:{server.server_address[1]}", flush=True)
server.serve_forever()
PY

log "starting the stand-in upstream"
upstream_addr_file="$test_root/upstream.addr"
python3 -I "$test_root/upstream.py" >"$upstream_addr_file" 2>"$test_root/upstream.log" &
upstream_pid=$!
waited=0
while [[ ! -s "$upstream_addr_file" ]]; do
	((waited < 100)) || fail "the stand-in upstream never started"
	sleep 0.1
	waited=$((waited + 1))
done
upstream_url="$(<"$upstream_addr_file")"
log "upstream on $upstream_url"

# shellcheck source=scripts/e2e/lib/faultproxy.sh
source "$(dirname "$0")/lib/faultproxy.sh"

faultproxy_start "$upstream_url"

reached_count() {
	command curl -fsS "$upstream_url/__count" |
		python3 -c 'import json,sys; print(json.load(sys.stdin)["reached"])'
}

# The two queries the agent really sends share a path. Only the one without a
# phase filter drives destructive cleanup, so a rule that cannot separate them
# would inject into the wrong call and still look like it worked.
log "checking the rule separates the ownership query from the Scheduled query"
faultproxy_arm '{
  "id": "ownership",
  "method": "GET",
  "path_prefix": "/api/v1/projects",
  "query_contains": ["nodeRef=node-a"],
  "query_absent": ["phase="],
  "respond": "status",
  "status": 503
}'

before="$(reached_count)"
status="$(command curl -s -o /dev/null -w '%{http_code}' \
	"$faultproxy_url/api/v1/projects?phase=Scheduled&nodeRef=node-a")"
[[ "$status" == "200" ]] || fail "the Scheduled query got $status, want 200: the rule must not match it"

status="$(command curl -s -o /dev/null -w '%{http_code}' \
	"$faultproxy_url/api/v1/projects?nodeRef=node-a")"
[[ "$status" == "503" ]] || fail "the ownership query got $status, want the injected 503"

after="$(reached_count)"
(( after - before == 1 )) ||
	fail "the upstream saw $((after - before)) requests, want only the Scheduled one"
log "ok: only the ownership query was injected into"

command curl -fsS -X POST "$faultproxy_control/control/reset" >/dev/null

# The synchronous handshake. The request freezes inside the proxy, the scenario
# does its work, and only then does the failure land. Both sides are at a known
# point, which is what makes the timing reproducible instead of racy.
log "checking a held request stays frozen until the scenario releases it"
faultproxy_arm '{
  "id": "ownership",
  "method": "GET",
  "path_prefix": "/api/v1/projects",
  "query_contains": ["nodeRef=node-a"],
  "query_absent": ["phase="],
  "hold": true,
  "respond": "status",
  "status": 503,
  "times": 1
}'

held_status_file="$test_root/held.status"
(
	command curl -s -o /dev/null -w '%{http_code}' --max-time 120 \
		"$faultproxy_url/api/v1/projects?nodeRef=node-a" >"$held_status_file"
) &
held_curl_pid=$!

event="$(faultproxy_wait ownership 30s)"
printf '%s\n' "$event" | grep -q '"kind":"held"' ||
	fail "the wait call did not report a held request: $event"
printf '%s\n' "$event" | grep -q 'nodeRef=node-a' ||
	fail "the held event is not the ownership query: $event"
log "ok: the ownership query is frozen inside the proxy"

# Stand-in for the state a scenario builds while the call is frozen, such as
# moving a Project's ownership to another node.
sleep 0.5
if ! kill -0 "$held_curl_pid" 2>/dev/null; then
	fail "the request finished while it should have been frozen"
fi
log "ok: the setup window stayed open"

before="$(reached_count)"
faultproxy_release ownership
wait "$held_curl_pid" || true
[[ "$(<"$held_status_file")" == "503" ]] ||
	fail "the released request got $(<"$held_status_file"), want the injected 503"
after="$(reached_count)"
(( after == before )) ||
	fail "the injected failure was forwarded upstream; the fault must come from the proxy"

kinds="$(faultproxy_event_kinds | tr '\n' ',')"
[[ "$kinds" == "held,released," ]] ||
	fail "event order was [$kinds], want [held,released,]"
log "ok: released as an injected failure, in that order"

command curl -fsS -X POST "$faultproxy_control/control/reset" >/dev/null

# The agent's own HTTP client times out (10s at the time of writing), so a
# request held too long fails without the proxy answering. A scenario that
# cannot see the difference would report a pass for a fault it never delivered.
log "checking a caller that gives up while held is recorded as such"
faultproxy_arm '{
  "id": "ownership",
  "method": "GET",
  "path_prefix": "/api/v1/projects",
  "query_contains": ["nodeRef=node-a"],
  "query_absent": ["phase="],
  "hold": true
}'

command curl -s -o /dev/null --max-time 1 \
	"$faultproxy_url/api/v1/projects?nodeRef=node-a" || true
waited=0
until [[ "$(faultproxy_event_kinds | tr '\n' ',')" == "held,client_gone," ]]; do
	((waited < 50)) || fail "the proxy did not record that the caller gave up: [$(faultproxy_event_kinds | tr '\n' ',')]"
	sleep 0.1
	waited=$((waited + 1))
done
log "ok: a caller's own timeout is a different event from an injected failure"

log "PASS"
