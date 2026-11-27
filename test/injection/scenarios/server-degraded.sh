#!/usr/bin/env bash
# Scenario: while the server is half-broken, the agent must not touch the
# workloads it runs.
#
# "Half-broken" means the server is up but its answers about ownership cannot
# be trusted: it answers 500, answers 200 with a payload the agent cannot read,
# or loses its database. In all three the agent does not know what it owns, and
# safety rule R8 says unknown ownership never counts toward stopping anything.
#
# Real binaries on one machine: cara-server, cara-agent, PostgreSQL in Docker,
# and the workload daemon in Docker-in-Docker. The HTTP fault proxy sits between
# the agent and the server, so the faults need no change to product code.
#
# Phases, each with its own fresh Project so one phase's damage cannot hide or
# cause another's:
#   P1 the ownership query answers 500 for 60 s           container must survive
#   P2 the ownership query answers truncated JSON, 60 s   container must survive
#   P3 PostgreSQL is stopped for 45 s                     container must survive
#   P4 control: the server says "you own nothing"         container MUST stop
#
# P4 shows the scenario can see a stop. Without it, a scenario whose container
# check was broken would pass P1-P3 for the wrong reason.
#
# Usage, from the repository root:
#   test/injection/scenarios/server-degraded.sh                 # cara as committed
#   test/injection/scenarios/server-degraded.sh --mutant <patch>
#   test/injection/scenarios/server-degraded.sh --timescale 0.1 [--mutant <patch>]
#
# --timescale applies test/injection/timescale.patch and runs both binaries
# with CARA_TIME_SCALE set, so every liveness, cleanup and recovery deadline
# shrinks by that factor. The fault windows below shrink by the same factor:
# they are sized in poll intervals, and the poll interval is one of the
# deadlines that shrank.
#
# The binaries are built from `git archive HEAD` in a temp directory, with the
# patch applied there, so the working tree is never mutated.
set -Eeuo pipefail

log() { printf '[server-degraded] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }

mutant=""
scale=1
while (($#)); do
	case "$1" in
	--mutant) mutant="$(realpath "$2")"; [[ -f "$mutant" ]] || fail "no such patch: $2"; shift 2 ;;
	--timescale) scale="$2"; shift 2 ;;
	*) fail "unknown argument: $1" ;;
	esac
done
# win <seconds> scales a fault window, never below 1 s.
win() { python3 -c 'import math, sys; print(max(1, math.ceil(float(sys.argv[1]) * float(sys.argv[2]))))' "$1" "$scale"; }
started=$SECONDS

for command in docker curl go python3 git; do
	command -v "$command" >/dev/null 2>&1 || fail "missing required command: $command"
done
cd "$(git rev-parse --show-toplevel)"
repo_root="$PWD"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/cara-server-degraded.XXXXXX")"
. "$repo_root/scripts/e2e/lib/auth.sh"
. "$repo_root/scripts/e2e/lib/faultproxy.sh"
suffix="${PPID}-$$"
postgres_name="cara-sd-postgres-${suffix}"
dind_name="cara-sd-dind-${suffix}"
server_pid=""
agent_pid=""
results=()

cleanup() {
	status=$?
	trap - EXIT INT TERM
	faultproxy_stop 2>/dev/null || true
	for pid in "$agent_pid" "$server_pid"; do
		if [[ -n "$pid" ]]; then
			kill "$pid" >/dev/null 2>&1 || true
			wait "$pid" >/dev/null 2>&1 || true
		fi
	done
	docker rm -f "$dind_name" "$postgres_name" >/dev/null 2>&1 || true
	if [[ "${KEEP_E2E_ARTIFACTS:-0}" == "1" ]]; then
		log "artifacts retained at $test_root"
	else
		case "$(basename "$test_root")" in
		cara-server-degraded.*) rm -rf -- "$test_root" ;;
		*) log "refusing to remove unexpected temp path: $test_root" ;;
		esac
	fi
	exit "$status"
}
trap cleanup EXIT INT TERM

wait_until() {
	local timeout_seconds=$1 description=$2
	shift 2
	local deadline=$((SECONDS + timeout_seconds))
	until "$@"; do
		((SECONDS < deadline)) || fail "timed out waiting for $description"
		sleep 1
	done
}

choose_port() {
	python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

dind() { env -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH docker -H "$dind_host" "$@"; }
dind_ready() { dind info >/dev/null 2>&1; }
server_ready() { curl -fsS "$server_url/api/v1/nodes" >/dev/null 2>&1; }
container_running() { [[ "$(dind inspect -f '{{.State.Running}}' "$1" 2>/dev/null || true)" == "true" ]]; }
container_stopped() { ! container_running "$1"; }
node_ready() {
	local body
	body="$(curl -fsS "$server_url/api/v1/nodes/node-a" 2>/dev/null || true)"
	[[ "$body" == *'"state":"Ready"'* ]]
}
project_is() {
	local body
	body="$(curl -fsS "$server_url/api/v1/projects/$1" 2>/dev/null || true)"
	[[ "$body" == *"\"phase\":\"$2\""* && "$body" == *"\"nodeRef\":\"$3\""* ]]
}

# matched_count <rule> counts the requests the proxy answered for that rule, so
# a phase can prove its fault reached the agent rather than pass on silence.
matched_count() {
	faultproxy_events | python3 -c '
import json, sys
print(sum(1 for e in json.load(sys.stdin) if e["kind"] == "matched" and e.get("rule_id") == sys.argv[1]))
' "$1"
}

# ownership_rule <id> <status> <body> answers every "what do I still own" query
# from node-a. The Scheduled query shares the path and is left alone.
ownership_rule() {
	python3 -c '
import json, sys
print(json.dumps({
    "id": sys.argv[1], "method": "GET", "path_prefix": "/api/v1/projects",
    "query_contains": ["nodeRef=node-a"], "query_absent": ["phase="],
    "respond": "status", "status": int(sys.argv[2]), "body": sys.argv[3],
}))' "$1" "$2" "$3"
}

# start_project <name> creates a one-container Project, waits until it runs on
# node-a, and prints its container's ID.
start_project() {
	curl -fsS -X POST "$server_url/api/v1/projects" -H 'Content-Type: application/json' -d '{
		"apiVersion":"caravanserai/v1","kind":"Project",
		"metadata":{"name":"'"$1"'","namespace":"default"},
		"spec":{"services":[{"name":"web","image":"nginx:alpine"}]}
	}' >/dev/null
	wait_until 90 "$1 Running on node-a" project_is "$1" Running node-a
	wait_until 30 "$1-web running" container_running "$1-web"
	dind inspect -f '{{.Id}}' "$1-web"
}

# watch_container <name> <id> <seconds> checks every second that the same
# container is still running. It prints the first second it was not, or
# nothing if it survived the whole window.
#
# A stopped container ends the window early, so a phase that fails may have
# seen only one faulty answer. That is a FAIL, not an INVALID: the fault landed
# and the agent acted on it. INVALID is kept for a survivor that was barely
# asked, which proves nothing.
watch_container() {
	local name=$1 id=$2 seconds=$3 i got
	for ((i = 1; i <= seconds; i++)); do
		sleep 1
		got="$(dind inspect -f '{{.Id}} {{.State.Running}}' "$name" 2>/dev/null || echo "absent")"
		if [[ "$got" != "$id true" ]]; then
			echo "$i"
			return
		fi
	done
}

record() { results+=("$1|$2|$3"); log "$1: $2 ($3)"; }

# ---------------------------------------------------------------- setup

src="$test_root/src"
mkdir -p "$src" "$test_root/bin" "$test_root/server-run" "$test_root/agent-run" "$test_root/agent-data"
git archive HEAD | tar -x -C "$src"
if [[ "$scale" != 1 ]]; then
	(cd "$src" && git apply "$repo_root/test/injection/timescale.patch") || fail "the timescale patch does not apply"
	export CARA_TIME_SCALE="$scale"
	log "deadlines scaled by $scale"
fi
if [[ -n "$mutant" ]]; then
	(cd "$src" && git apply "$mutant") || fail "the patch does not apply to HEAD: $mutant"
	log "built with mutant $(basename "$mutant")"
else
	log "built from HEAD $(git rev-parse --short HEAD), no mutant"
fi
(cd "$src" && go build -o "$test_root/bin/cara-server" ./cmd/cara-server &&
	go build -o "$test_root/bin/cara-agent" ./cmd/cara-agent) || fail "build failed"

postgres_port="$(choose_port)"
log "starting PostgreSQL ($postgres_name) on 127.0.0.1:$postgres_port"
docker run -d --name "$postgres_name" \
	-e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=password -e POSTGRES_DB=caravanserai \
	-p "127.0.0.1:$postgres_port:5432" "${POSTGRES_IMAGE:-postgres:16}" >/dev/null
wait_until 60 "PostgreSQL readiness" docker exec "$postgres_name" pg_isready -U postgres

log "starting the workload Docker daemon ($dind_name)"
data_root="$test_root/agent-data"
docker run -d --privileged --name "$dind_name" -e DOCKER_TLS_CERTDIR= \
	-p 127.0.0.1::2375 -v "$data_root:$data_root" \
	"${DIND_IMAGE:-docker:27-dind}" --host=tcp://0.0.0.0:2375 --tls=false >/dev/null
dind_host="tcp://127.0.0.1:$(docker port "$dind_name" 2375/tcp | sed 's/.*://')"
wait_until 90 "Docker-in-Docker readiness" dind_ready
dind pull nginx:alpine >/dev/null

server_port="$(choose_port)"
server_url="http://127.0.0.1:$server_port"
(
	cd "$test_root/server-run"
	env HOST=127.0.0.1 PORT="$server_port" \
		BREAK_GLASS_TOKEN_FILE="$e2e_admin_token_file" \
		DATABASE_URL="postgresql://postgres:password@127.0.0.1:$postgres_port/caravanserai?sslmode=disable" \
		"$test_root/bin/cara-server"
) >"$test_root/server.log" 2>&1 &
server_pid=$!
wait_until 60 "cara-server readiness" server_ready

faultproxy_start "$server_url"

printf 'data_root: "%s"\n' "$data_root" >"$test_root/agent-run/config.yaml"
agent_token_file="$(join_token_file node-a)"
(
	cd "$test_root/agent-run"
	env -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH \
		SERVER_URL="$faultproxy_url" JOIN_TOKEN_FILE="$agent_token_file" \
		NODE_NAME=node-a HEARTBEAT_INTERVAL=1s DOCKER_HOST="$dind_host" \
		AGENT_LISTEN_PORT=0 PROXY_LISTEN_ADDR=127.0.0.1:0 AGENT_ADVERTISE_IP=127.0.0.1 \
		"$test_root/bin/cara-agent"
) >"$test_root/agent.log" 2>&1 &
agent_pid=$!
wait_until 60 "node-a Ready" node_ready

setup_secs=$((SECONDS - started))
phases_started=$SECONDS

# ---------------------------------------------------------------- P1

log "P1: the ownership query answers 500 for $(win 60) s"
id="$(start_project p1-target)"
faultproxy_arm "$(ownership_rule p1-500 500 'internal error')"
stopped_at="$(watch_container p1-target-web "$id" "$(win 60)")"
faultproxy_disarm p1-500
n="$(matched_count p1-500)"
if [[ -n "$stopped_at" ]] && ((n >= 1)); then
	record P1 FAIL "container stopped ${stopped_at}s into the fault; $n queries answered 500 by then"
elif ((n < 3)); then
	record P1 INVALID "the agent asked only $n times; the fault never really landed"
else
	record P1 PASS "container survived; $n queries answered 500"
fi

# ---------------------------------------------------------------- P2

log "P2: the ownership query answers 200 with truncated JSON for $(win 60) s"
id="$(start_project p2-target)"
faultproxy_arm "$(ownership_rule p2-truncated 200 '{"items":[')"
stopped_at="$(watch_container p2-target-web "$id" "$(win 60)")"
faultproxy_disarm p2-truncated
n="$(matched_count p2-truncated)"
if [[ -n "$stopped_at" ]] && ((n >= 1)); then
	record P2 FAIL "container stopped ${stopped_at}s into the fault; $n queries got truncated JSON by then"
elif ((n < 3)); then
	record P2 INVALID "the agent asked only $n times; the fault never really landed"
else
	record P2 PASS "container survived; $n queries got truncated JSON"
fi

# ---------------------------------------------------------------- P3

log "P3: PostgreSQL is stopped for $(win 45) s (under the $(win 90) s heartbeat timeout)"
id="$(start_project p3-target)"
errors_before="$(grep -c 'Failed to list projects for reconcile' "$test_root/agent.log" || true)"
docker stop -t 1 "$postgres_name" >/dev/null
stopped_at="$(watch_container p3-target-web "$id" "$(win 45)")"
docker start "$postgres_name" >/dev/null
wait_until 60 "PostgreSQL back" docker exec "$postgres_name" pg_isready -U postgres
errors=$(($(grep -c 'Failed to list projects for reconcile' "$test_root/agent.log" || true) - errors_before))
if [[ -n "$stopped_at" ]] && ((errors >= 1)); then
	record P3 FAIL "container stopped ${stopped_at}s into the outage; $errors failed listings by then"
elif ((errors < 2)); then
	record P3 INVALID "the agent logged only $errors failed listings; the outage never really landed"
else
	record P3 PASS "container survived; $errors failed listings"
fi
wait_until 120 "node-a Ready after the outage" node_ready

# ---------------------------------------------------------------- P4

log "P4 (control): the server says node-a owns nothing; the container must stop"
id="$(start_project p4-target)"
faultproxy_arm "$(ownership_rule p4-empty 200 '{"items":[]}')"
stopped_at="$(watch_container p4-target-web "$id" "$(win 60)")"
faultproxy_disarm p4-empty
if [[ -n "$stopped_at" ]]; then
	record P4 PASS "container stopped ${stopped_at}s after ownership was withdrawn: the check sees stops"
else
	record P4 FAIL "container still running after $(win 60) s; the scenario cannot see a stop, so P1-P3 prove nothing"
fi

# ---------------------------------------------------------------- summary

echo
log "timing: setup ${setup_secs}s, phases $((SECONDS - phases_started))s, total $((SECONDS - started))s (scale $scale)"
printf '%-4s %-8s %s\n' PHASE VERDICT DETAIL
failed=0
for r in "${results[@]}"; do
	IFS='|' read -r phase verdict detail <<<"$r"
	printf '%-4s %-8s %s\n' "$phase" "$verdict" "$detail"
	[[ "$verdict" == PASS ]] || failed=1
done
exit "$failed"
