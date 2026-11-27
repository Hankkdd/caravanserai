# shellcheck shell=bash
# vmtest-proves: With the server's default settings, a Project whose Node is cut off past the grace period stays assigned to that Node and asks for an authorized takeover instead of being moved, because nothing confirmed its workload stopped (GraceExpiredRelease is off by default)
# vmtest-needs:
# vmtest-server-env: RUNNING_GRACE_PERIOD=1m
# vmtest-power-offs: 0
# vmtest-minutes: 15
# vmtest-enrol: auto
#
# The counterpart of overlap-partition, which turns GRACE_EXPIRED_RELEASE on
# and shows the move. This one leaves it at its default and shows the refusal.
# Only RUNNING_GRACE_PERIOD is shortened, so the run does not wait 3 minutes.
#
# Added for the 2026 software-testing course project (Group 42). It is aimed at
# a wiring defect no unit or integration test can reach: cmd/cara-server
# passing a hard-coded value instead of cfg.GraceExpiredRelease. Those tests
# build the components directly and never run main().

P=dg-allow

manifest() {
	printf 'apiVersion: caravanserai/v1\nkind: Project\nmetadata:\n  name: %s\nspec:\n  size: Small\n  services:\n    - name: app\n      image: busybox:1.36\n      command: ["sh", "-c", "exec sleep 86400"]\n' "$1" |
		srv "cat > /tmp/$1.yaml"
	ctl "apply -f /tmp/$1.yaml" >/dev/null
}

node_of() { field "$1" .status.nodeRef; }
phase_of() { field "$1" .status.phase; }
running_on() { [[ "$(phase_of "$1")" == Running && "$(node_of "$1")" == "$2" ]]; }
copies() { vm "$1" "sudo docker ps -q --filter label=cara.project=$2 --filter status=running | wc -l"; }
node_state() { api "/api/v1/nodes/$1" 2>/dev/null | jq -r '.status.state // ""'; }
node_is() { [[ "$(node_state "$1")" == "$2" ]]; }
# decided: the server has either asked for a takeover or moved the Project.
# Waiting for one of the two, not for the expected one, keeps a wrong decision
# from looking like "not decided yet".
decided() { [[ -n "$(cond "$P" TakeoverRequired)" || "$(node_of "$P")" != node-a ]]; }

va=$(vm_for_node node-a)
vb=$(vm_for_node node-b)
cut() { vm "$va" "sudo iptables -I OUTPUT -d $VMTEST_SERVER_IP -j DROP && sudo iptables -I INPUT -s $VMTEST_SERVER_IP -j DROP"; }
heal() { vm "$va" "sudo iptables -D OUTPUT -d $VMTEST_SERVER_IP -j DROP; sudo iptables -D INPUT -s $VMTEST_SERVER_IP -j DROP" || true; }

log "--- S1: the Project runs on node-a"
observe "S1 server's GRACE_EXPIRED_RELEASE setting" \
	"$(srv "sudo grep -h '^GRACE_EXPIRED_RELEASE=' /etc/cara/server.env 2>/dev/null | tail -1" || true)"
ctl "node set-tier node-a primary" >/dev/null
ctl "node set-tier node-b backup" >/dev/null
ctl "delete project $P --force" >/dev/null 2>&1 || true
manifest "$P"
wait_for 300 "$P Running on node-a" running_on "$P" node-a
expect "S1 Project placed on node-a" node-a/Running "$(node_of "$P")/$(phase_of "$P")"
if [[ "$(node_of "$P")" != node-a ]]; then
	log "the Project must start on node-a for the partition to mean anything; stopping"
	ctl "delete project $P --force" >/dev/null 2>&1 || true
	return 0
fi

log "--- S2: cut node-a off from the server"
cut
t0=$SECONDS
if wait_for 240 "node-a NotReady" node_is node-a NotReady; then
	observe "S2 seconds from the cut to node-a NotReady" "$((SECONDS - t0))"
fi
expect "S2 node-a is NotReady" NotReady "$(node_state node-a)"

log "--- S3: past the grace period the server decides"
if wait_for 420 "the server to decide about $P" decided; then
	observe "S3 seconds from the cut to the decision" "$((SECONDS - t0))"
fi
# Give a wrong move time to show: a release can land after the condition.
sleep 60
expect "S3 Project still assigned to node-a" node-a "$(node_of "$P")"
expect "S3 server asks for an authorized takeover" WorkloadStopUnconfirmed "$(cond "$P" TakeoverRequired)"
expect "S3 Project has no copy on node-b" 0 "$(copies "$vb" "$P")"
expect "S3 Project keeps running on cut-off node-a" 1 "$(copies "$va" "$P")"
observe "S3 Project phase while node-a is cut off" "$(phase_of "$P")"

log "--- S4: node-a comes back"
heal
t4=$SECONDS
if wait_for 240 "node-a Ready" node_is node-a Ready; then
	observe "S4 seconds from healing to node-a Ready" "$((SECONDS - t4))"
fi
expect "S4 node-a is Ready" Ready "$(node_state node-a)"
wait_for 120 "$P Running on node-a" running_on "$P" node-a || true
expect "S4 Project Running on node-a" node-a/Running "$(node_of "$P")/$(phase_of "$P")"
expect "S4 Project has one copy on node-a" 1 "$(copies "$va" "$P")"
expect "S4 Project has no copy on node-b" 0 "$(copies "$vb" "$P")"

log "--- cleanup"
heal
ctl "delete project $P --force" >/dev/null 2>&1 || true
