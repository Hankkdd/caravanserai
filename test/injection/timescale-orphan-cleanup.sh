#!/usr/bin/env bash
# Runs cara's own scripts/e2e/orphan-cleanup.sh, optionally with deadlines
# scaled and a mutant applied, and reports how long it took.
#
#   test/injection/timescale-orphan-cleanup.sh [--timescale F] [--mutant <patch>]
#
# Everything happens in a temp copy of HEAD (git archive), so the working tree
# is never changed. With --timescale the copy gets test/injection/timescale.patch,
# CARA_TIME_SCALE is exported (the script's server and agent inherit it), and
# the script's one fixed wait - `sleep 120`, two thirds of the agent's 3-minute
# deletion grace - is scaled by the same factor. Its wait_until calls are upper
# bounds, not waits, and are left alone.
set -Eeuo pipefail

scale=1
mutant=""
while (($#)); do
	case "$1" in
	--timescale) scale="$2"; shift 2 ;;
	--mutant) mutant="$(realpath "$2")"; shift 2 ;;
	*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

cd "$(git rev-parse --show-toplevel)"
repo_root="$PWD"
tree="$(mktemp -d "${TMPDIR:-/tmp}/cara-ts-orphan.XXXXXX")"
trap 'case "$(basename "$tree")" in cara-ts-orphan.*) rm -rf -- "$tree" ;; esac' EXIT
git archive HEAD | tar -x -C "$tree"

if [[ "$scale" != 1 ]]; then
	(cd "$tree" && git apply "$repo_root/test/injection/timescale.patch")
	wait120="$(python3 -c 'import math, sys; print(max(1, math.ceil(120 * float(sys.argv[1]))))' "$scale")"
	grep -q '^sleep 120$' "$tree/scripts/e2e/orphan-cleanup.sh" || { echo "orphan-cleanup.sh changed: no 'sleep 120' to scale" >&2; exit 2; }
	sed -i "s/^sleep 120\$/sleep $wait120/" "$tree/scripts/e2e/orphan-cleanup.sh"
	export CARA_TIME_SCALE="$scale"
fi
[[ -n "$mutant" ]] && (cd "$tree" && git apply "$mutant")

echo "[timescale-orphan] scale $scale; mutant ${mutant:+$(basename "$mutant")}${mutant:-none}"
start=$SECONDS
status=0
(cd "$tree" && bash scripts/e2e/orphan-cleanup.sh) || status=$?
echo "[timescale-orphan] exit $status after $((SECONDS - start))s (scale $scale)"
exit "$status"
