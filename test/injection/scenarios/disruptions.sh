#!/usr/bin/env bash
# Bug hunt: everyday disruptions no existing test exercises.
#
#   D1 cara-server is down for SERVER_DOWN seconds (default 240, past the 90 s
#      heartbeat timeout) and comes back. Nothing failed on node-a, so no
#      Project may be moved off it, asked for a takeover, or have its
#      container replaced.
#   D2 while cara-server is down, cara-agent restarts (a site power cut where
#      the node boots before the server). The agent must stay up, leave the
#      containers alone, and reconnect once the server is back.
#   D3 a Project is deleted and recreated under the same name on the same node
#      (defect 26 in the issue log was exactly this; checked again on main).
#
# Real binaries on one machine, like server-degraded.sh. node-b is a fake: the
# script registers it and heartbeats for it, so a wrong release has somewhere
# to go and shows up as a changed nodeRef.
#
# Usage, from the repository root:
#   test/injection/scenarios/disruptions.sh [git-ref]        # default HEAD
#   SERVER_ENV="GRACE_EXPIRED_RELEASE=true RUNNING_GRACE_PERIOD=1m" \
#     test/injection/scenarios/disruptions.sh origin/main
set -Eeuo pipefail

log() { printf '[disruptions] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }

ref="${1:-HEAD}"
server_down="${SERVER_DOWN:-240}"
cd "$(git rev-parse --show-toplevel)"
repo_root="$PWD"
commit="$(git rev-parse --short "$ref")"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/cara-disruptions.XXXXXX")"
. "$repo_root/scripts/e2e/lib/auth.sh"
suffix="${PPID}-$$"
postgres_name="cara-dr-postgres-${suffix}"
dind_name="cara-dr-dind-${suffix}"
server_pid=""
agent_pid=""
node_b_pid=""
results=()

cleanup() {
	status=$?
	trap - EXIT INT TERM
	for pid in "$node_b_pid" "$agent_pid" "$server_pid"; do
		[[ -n "$pid" ]] && { kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; }
	done
	docker rm -f "$dind_name" "$postgres_name" >/dev/null 2>&1 || true
	if [[ "${KEEP_E2E_ARTIFACTS:-0}" == "1" ]]; then
		log "artifacts retained at $test_root"
	else
		case "$(basename "$test_root")" in
		cara-disruptions.*) rm -rf -- "$test_root" ;;
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
		((SECONDS < deadline)) || { log "timed out waiting for $description"; return 1; }
		sleep 1
	done
}
choose_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'; }
dind() { env -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH docker -H "$dind_host" "$@"; }
dind_ready() { dind info >/dev/null 2>&1; }
server_ready() { curl -fsS "$server_url/api/v1/nodes" >/dev/null 2>&1; }
container_id() { dind inspect -f '{{if .State.Running}}{{.Id}}{{else}}stopped{{end}}' "$1-web" 2>/dev/null || echo absent; }
running_container() { [[ "$(container_id "$1")" != absent && "$(container_id "$1")" != stopped ]]; }
project_json() { curl -fsS "$server_url/api/v1/projects/$1" 2>/dev/null || true; }
node_state() { curl -fsS "$server_url/api/v1/nodes/$1" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"].get("state",""))' 2>/dev/null || true; }
node_is() { [[ "$(node_state "$1")" == "$2" ]]; }
# summary <project> prints "phase nodeRef takeover-reason".
summary() {
	project_json "$1" | python3 -c '
import json, sys
try:
    s = json.load(sys.stdin)["status"]
except Exception:
    print("? ? ?"); sys.exit()
t = [c.get("reason", "") for c in s.get("conditions", []) if c["type"] == "TakeoverRequired" and c["status"] == "True"]
print(s.get("phase", "?"), s.get("nodeRef", "") or "-", t[0] if t else "-")'
}
running_on_a() { [[ "$(summary "$1")" == "Running node-a -" ]]; }
create_project() {
	curl -fsS -X POST "$server_url/api/v1/projects" -H 'Content-Type: application/json' -d '{
		"apiVersion":"caravanserai/v1","kind":"Project",
		"metadata":{"name":"'"$1"'","namespace":"default"},
		"spec":{"services":[{"name":"web","image":"nginx:alpine"}]}}' >/dev/null
}
record() { results+=("$1|$2|$3"); log "$1: $2 ($3)"; }

start_server() {
	(
		cd "$test_root/server-run"
		# shellcheck disable=SC2086
		env HOST=127.0.0.1 PORT="$server_port" BREAK_GLASS_TOKEN_FILE="$e2e_admin_token_file" \
			DATABASE_URL="postgresql://postgres:password@127.0.0.1:$postgres_port/caravanserai?sslmode=disable" \
			${SERVER_ENV:-} "$test_root/bin/cara-server"
	) >>"$test_root/server.log" 2>&1 &
	server_pid=$!
	wait_until 60 "cara-server readiness" server_ready || fail "cara-server did not come up"
}
stop_server() { kill "$server_pid"; wait "$server_pid" 2>/dev/null || true; server_pid=""; }
start_agent() {
	(
		cd "$test_root/agent-run"
		env -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH \
			SERVER_URL="$server_url" JOIN_TOKEN_FILE="$agent_token_file" NODE_NAME=node-a \
			DOCKER_HOST="$dind_host" AGENT_LISTEN_PORT=0 PROXY_LISTEN_ADDR=127.0.0.1:0 \
			AGENT_ADVERTISE_IP=127.0.0.1 "$test_root/bin/cara-agent"
	) >>"$test_root/agent.log" 2>&1 &
	agent_pid=$!
}
stop_agent() { kill "$agent_pid"; wait "$agent_pid" 2>/dev/null || true; agent_pid=""; }

# ---------------------------------------------------------------- setup
# The agent keeps its default 30 s heartbeat: the gap between the server's
# restart and the first heartbeat is part of what D1 is about.

mkdir -p "$test_root/src" "$test_root/bin" "$test_root/server-run" "$test_root/agent-run" "$test_root/agent-data"
git archive "$ref" | tar -x -C "$test_root/src"
log "built from $ref ($commit); server env: ${SERVER_ENV:-(defaults)}"
(cd "$test_root/src" && go build -o "$test_root/bin/cara-server" ./cmd/cara-server &&
	go build -o "$test_root/bin/cara-agent" ./cmd/cara-agent) || fail "build failed"

postgres_port="$(choose_port)"
docker run -d --name "$postgres_name" -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=password \
	-e POSTGRES_DB=caravanserai -p "127.0.0.1:$postgres_port:5432" "${POSTGRES_IMAGE:-postgres:16}" >/dev/null
wait_until 60 "PostgreSQL" docker exec "$postgres_name" pg_isready -U postgres >/dev/null || fail "no PostgreSQL"
data_root="$test_root/agent-data"
docker run -d --privileged --name "$dind_name" -e DOCKER_TLS_CERTDIR= -p 127.0.0.1::2375 \
	-v "$data_root:$data_root" "${DIND_IMAGE:-docker:27-dind}" --host=tcp://0.0.0.0:2375 --tls=false >/dev/null
dind_host="tcp://127.0.0.1:$(docker port "$dind_name" 2375/tcp | sed 's/.*://')"
wait_until 90 "Docker-in-Docker" dind_ready || fail "no Docker-in-Docker"
dind pull nginx:alpine >/dev/null

server_port="$(choose_port)"
server_url="http://127.0.0.1:$server_port"
start_server
printf 'data_root: "%s"\n' "$data_root" >"$test_root/agent-run/config.yaml"
agent_token_file="$(join_token_file node-a)"
start_agent
wait_until 90 "node-a Ready" node_is node-a Ready || fail "node-a never Ready"

for p in d1-a d1-b d3-same; do create_project "$p"; done
for p in d1-a d1-b d3-same; do
	wait_until 120 "$p Running on node-a" running_on_a "$p" || fail "$p never ran on node-a"
	wait_until 30 "$p container" running_container "$p" || fail "$p has no container"
done

# node-b exists only so a wrong release has a destination.
curl -fsS -X POST "$server_url/api/v1/nodes" -H 'Content-Type: application/json' \
	-d '{"apiVersion":"caravanserai/v1","kind":"Node","metadata":{"name":"node-b"},"spec":{"hostname":"node-b"}}' >/dev/null
(
	while true; do
		curl -fsS -X POST "$server_url/api/v1/nodes/node-b/heartbeat" -H 'Content-Type: application/json' \
			-d '{"state":"Ready","network":{"overlayIP":"127.0.0.1","agentPort":0}}' >/dev/null 2>&1 || true
		sleep 5
	done
) &
node_b_pid=$!
wait_until 30 "node-b Ready" node_is node-b Ready || fail "node-b never Ready"

# ---------------------------------------------------------------- D1

log "D1: cara-server down for ${server_down}s, then back"
id_a="$(container_id d1-a)"
id_b="$(container_id d1-b)"
stop_server
sleep "$server_down"
start_server
restarted=$SECONDS
worst="" notready_seen=0 notready_last=0
while ((SECONDS - restarted < 180)); do
	for p in d1-a d1-b; do
		s="$(summary "$p")"
		[[ "$s" == "Running node-a -" || "$s" == "? ? ?" ]] || worst="${worst:-$p: $s at +$((SECONDS - restarted))s}"
	done
	if node_is node-a NotReady; then
		notready_seen=1
		notready_last=$((SECONDS - restarted))
	fi
	sleep 2
done
[[ "$notready_seen" == 1 ]] && log "D1 observed: node-a was NotReady after the restart, last seen at +${notready_last}s"
final="$(summary d1-a) / $(summary d1-b)"
same="$([[ "$(container_id d1-a)" == "$id_a" && "$(container_id d1-b)" == "$id_b" ]] && echo yes || echo no)"
if [[ -n "$worst" ]]; then
	record D1 FAIL "first deviation: $worst; final: $final; containers unchanged: $same"
elif [[ "$same" != yes ]]; then
	record D1 FAIL "assignments kept, but a container was replaced or stopped"
else
	record D1 PASS "both stayed Running on node-a, containers unchanged (node-a NotReady after restart: $([[ $notready_seen == 1 ]] && echo "yes, until +${notready_last}s" || echo no))"
fi

# ---------------------------------------------------------------- D2

log "D2: cara-agent restarts while cara-server is down"
id_a="$(container_id d1-a)"
stop_server
stop_agent
start_agent
sleep 60
alive="$(kill -0 "$agent_pid" 2>/dev/null && echo yes || echo no)"
same_down="$([[ "$(container_id d1-a)" == "$id_a" ]] && echo yes || echo no)"
start_server
back=no
wait_until 120 "node-a Ready after the server returns" node_is node-a Ready && back=yes
wait_until 60 "d1-a Running on node-a" running_on_a d1-a || true
same_up="$([[ "$(container_id d1-a)" == "$id_a" ]] && echo yes || echo no)"
if [[ "$alive" == yes && "$same_down" == yes && "$back" == yes && "$same_up" == yes ]]; then
	record D2 PASS "agent stayed up with no server, container untouched, reconnected"
else
	record D2 FAIL "agent alive without server: $alive; container untouched while down: $same_down; node Ready again: $back; untouched after: $same_up; d1-a: $(summary d1-a)"
fi

# ---------------------------------------------------------------- D3

log "D3: delete d3-same and recreate it under the same name"
code="$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$server_url/api/v1/projects/d3-same")"
gone() { [[ "$(curl -sS -o /dev/null -w '%{http_code}' "$server_url/api/v1/projects/d3-same")" == 404 ]]; }
if ! wait_until 300 "d3-same deleted" gone; then
	record D3 FAIL "DELETE answered $code but the Project was still there after 300 s: $(summary d3-same)"
else
	t0=$SECONDS
	create_project d3-same
	if wait_until 180 "recreated d3-same Running on node-a" running_on_a d3-same &&
		wait_until 30 "recreated container" running_container d3-same; then
		record D3 PASS "recreated under the same name and Running in $((SECONDS - t0))s"
	else
		record D3 FAIL "recreated d3-same did not run within 180 s: $(summary d3-same), container $(container_id d3-same)"
	fi
fi

# ---------------------------------------------------------------- summary

echo
printf '%-4s %-8s %s\n' CASE VERDICT DETAIL
failed=0
for r in "${results[@]}"; do
	IFS='|' read -r c v d <<<"$r"
	printf '%-4s %-8s %s\n' "$c" "$v" "$d"
	[[ "$v" == PASS ]] || failed=1
done
exit "$failed"
