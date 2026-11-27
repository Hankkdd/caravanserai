# shellcheck shell=bash
# vmtest-proves: A cluster installed from one release keeps its Projects running and its data through an in-place package upgrade to VMTEST_UPGRADE_TO: server first, then the agents, with the mixed-version window in between
# vmtest-needs:
# vmtest-server-env:
# vmtest-power-offs: 0
# vmtest-minutes: 25
# vmtest-enrol: auto
#
# Run it with the OLD release as --commit and the new commit in the
# environment:
#
#   VMTEST_UPGRADE_TO=$(git rev-parse origin/main) \
#     test/vm/run.sh --commit v0.1.0-rc.1 upgrade-from-rc1
#
# ADR-0033 promises that upgrading a package restarts the running service.
# Nothing tests what that restart does to a cluster with state in it. An
# operator upgrades the server first and the agents later, so the scenario
# holds the mixed-version window open and uses the cluster in it.
#
# Expectations that the design states are `expect`. Behaviour the design does
# not pin down (whether the new agent recreates containers the old one made)
# is `observe`, so the record shows it without calling it a defect.
#
# Added for the 2026 software-testing course project (Group 42).

[[ -n "${VMTEST_UPGRADE_TO:-}" ]] || { log "VMTEST_UPGRADE_TO is not set; skipping"; return 0; }

S=up-stateless
M=up-managed
N=up-mixed
va=$(vm_for_node node-a)
vb=$(vm_for_node node-b)

stateless() {
	printf 'apiVersion: caravanserai/v1\nkind: Project\nmetadata:\n  name: %s\nspec:\n  size: Small\n  services:\n    - name: app\n      image: busybox:1.36\n      command: ["sh", "-c", "exec sleep 86400"]\n%b' "$1" "${2:-}" |
		srv "cat > /tmp/$1.yaml"
	ctl "apply -f /tmp/$1.yaml" >/dev/null
}
managed() {
	printf 'apiVersion: caravanserai/v1\nkind: Project\nmetadata:\n  name: %s\nspec:\n  size: Small\n  volumes:\n    - name: data\n      type: Managed\n  services:\n    - name: app\n      image: busybox:1.36\n      command: ["sh", "-c", "date >> /data/boots; exec sleep 86400"]\n      volumeMounts:\n        - name: data\n          mountPath: /data\n' "$1" |
		srv "cat > /tmp/$1.yaml"
	ctl "apply -f /tmp/$1.yaml" >/dev/null
}

node_of() { field "$1" .status.nodeRef; }
phase_of() { field "$1" .status.phase; }
running() { [[ "$(phase_of "$1")" == Running ]]; }
vm_of() { [[ "$(node_of "$1")" == node-b ]] && echo "$vb" || echo "$va"; }
cid() { vm "$(vm_of "$1")" "sudo docker inspect -f '{{.Id}}' $1-app 2>/dev/null" || true; }
crunning() { [[ "$(vm "$(vm_of "$1")" "sudo docker inspect -f '{{.State.Running}}' $1-app 2>/dev/null" || true)" == true ]]; }
node_state() { api "/api/v1/nodes/$1" 2>/dev/null | jq -r '.status.state // ""'; }
both_ready() { [[ "$(node_state node-a)/$(node_state node-b)" == Ready/Ready ]]; }
version_of() { vm "$1" "dpkg-query -W -f '\${Version}' $2"; }
boots() { vm "$(vm_of "$M")" "sudo docker exec $M-app cat /data/boots 2>/dev/null | wc -l" || true; }
marker() { vm "$(vm_of "$M")" "sudo docker exec $M-app cat /data/marker 2>/dev/null" || true; }

log "--- U1: the old release runs two Projects, one with Managed data"
observe "U1 cara-server version" "$(version_of "$VMTEST_SERVER" cara-server)"
observe "U1 cara-agent version on node-a" "$(version_of "$va" cara-agent)"
for p in "$S" "$M" "$N"; do ctl "delete project $p --force" >/dev/null 2>&1 || true; done
stateless "$S"
managed "$M"
wait_for 300 "$S Running" running "$S"
wait_for 300 "$M Running" running "$M"
expect "U1 stateless Project Running" Running "$(phase_of "$S")"
expect "U1 Managed Project Running" Running "$(phase_of "$M")"
vm "$(vm_of "$M")" "sudo docker exec $M-app sh -c 'echo before-upgrade > /data/marker'"
expect "U1 marker written into the Managed volume" before-upgrade "$(marker)"
s_node=$(node_of "$S") m_node=$(node_of "$M")
s_id=$(cid "$S") m_id=$(cid "$M")
observe "U1 placement" "$S on $s_node, $M on $m_node"

log "--- U2: upgrade cara-server and caractl only"
new="$(mktemp -d)"
if ! deploy_build "$VMTEST_UPGRADE_TO" "$new"; then
	expect "U2 new packages build" yes no
	rm -rf "$new"
	return 0
fi
t2=$(date -u '+%Y-%m-%d %H:%M:%S')
install_package "$VMTEST_SERVER" "$new" cara-server
install_package "$VMTEST_SERVER" "$new" caractl
observe "U2 cara-server version" "$(version_of "$VMTEST_SERVER" cara-server)"
expect "U2 cara-server active after the upgrade" active "$(vm "$VMTEST_SERVER" 'systemctl is-active cara-server' || true)"
wait_for 120 "the API answers" api /api/v1/nodes >/dev/null || true
# Two heartbeat timeouts: long enough for a refused old agent to show.
sleep 180
expect "U2 both nodes Ready with old agents" Ready/Ready "$(node_state node-a)/$(node_state node-b)"
expect "U2 stateless Project still Running where it was" "$s_node/Running" "$(node_of "$S")/$(phase_of "$S")"
expect "U2 Managed Project still Running where it was" "$m_node/Running" "$(node_of "$M")/$(phase_of "$M")"
expect "U2 stateless container untouched" "$s_id" "$(cid "$S")"
expect "U2 Managed container untouched" "$m_id" "$(cid "$M")"
srv "sudo journalctl -u cara-server --no-pager -o cat --since '$t2'" >"$VMTEST_OUT/logs/upgrade-server-after-u2.log" 2>&1 || true
observe "U2 server log lines with 'refus' or 'forbidden' since the upgrade" \
	"$(grep -ciE 'refus|forbidden' "$VMTEST_OUT/logs/upgrade-server-after-u2.log" || true)"
observe "U2 server log lines at error level since the upgrade" \
	"$(grep -c '"level":"error"' "$VMTEST_OUT/logs/upgrade-server-after-u2.log" || true)"
for n in node-a node-b; do
	observe "U2 $n agent warnings since the server upgrade" "$(agent_log_since "$(vm_for_node "$n")" "$t2" | grep -c '"level":"warn"' || true)"
done

log "--- U3: the mixed-version cluster takes a new Project"
stateless "$N"
if wait_for 240 "$N Running under old agents" running "$N"; then :; fi
expect "U3 new Project Running with a new server and old agents" Running "$(phase_of "$N")"

log "--- U4: upgrade the agents"
t4=$(date -u '+%Y-%m-%d %H:%M:%S')
for v in "$va" "$vb"; do
	install_package "$v" "$new" cara-agent
	expect "U4 cara-agent active on $v after the upgrade" active "$(vm "$v" 'systemctl is-active cara-agent' || true)"
done
observe "U4 cara-agent version on node-a" "$(version_of "$va" cara-agent)"
wait_for 120 "both nodes Ready" both_ready || true
sleep 60
expect "U4 both nodes Ready" Ready/Ready "$(node_state node-a)/$(node_state node-b)"
for p in "$S" "$M" "$N"; do
	wait_for 120 "$p Running" running "$p" || true
	expect "U4 $p Running" Running "$(phase_of "$p")"
	expect "U4 $p container running" yes "$(crunning "$p" && echo yes || echo no)"
done
expect "U4 Projects stayed on their nodes" "$s_node $m_node" "$(node_of "$S") $(node_of "$M")"
observe "U4 stateless container kept by the new agent" "$([[ "$(cid "$S")" == "$s_id" ]] && echo yes || echo "no, recreated")"
observe "U4 Managed container kept by the new agent" "$([[ "$(cid "$M")" == "$m_id" ]] && echo yes || echo "no, recreated")"
observe "U4 Managed container starts recorded in /data/boots" "$(boots)"
expect "U4 Managed data survived the upgrade" before-upgrade "$(marker)"
for n in node-a node-b; do
	agent_log_since "$(vm_for_node "$n")" "$t4" >"$VMTEST_OUT/logs/upgrade-agent-$n-after-u4.log" 2>&1 || true
	observe "U4 $n agent error lines since its upgrade" "$(grep -c '"level":"error"' "$VMTEST_OUT/logs/upgrade-agent-$n-after-u4.log" || true)"
done

log "--- U5: the upgraded cluster changes and deletes Projects"
old_id=$(cid "$S")
stateless "$S" '      env:\n        - name: UPGRADED\n          value: "yes"\n'
changed() { [[ "$(cid "$S")" != "$old_id" ]] && crunning "$S"; }
wait_for 240 "$S recreated with the new spec" changed || true
expect "U5 spec change applied to a Project created before the upgrade" yes "$(changed && echo yes || echo no)"
ctl "delete project $N" >/dev/null 2>&1 || true
gone() { [[ -z "$(project "$N")" ]]; }
wait_for 240 "$N deleted" gone || true
expect "U5 delete completes after the upgrade" yes "$(gone && echo yes || echo no)"

log "--- cleanup"
rm -rf "$new"
for p in "$S" "$M" "$N"; do ctl "delete project $p --force" >/dev/null 2>&1 || true; done
