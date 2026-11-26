#!/usr/bin/env bash
# Runs the existing test suites against each mutation patch and reports which
# layer, if any, kills it. The product code is restored after every mutant,
# including on Ctrl-C, so the committed tree is never left mutated.
#
# Usage: test/injection/mutations/run.sh [--integration] [patch ...]
#   --integration  also run make test-integration (needs Docker, ~50s) for
#                  mutants that survive the unit suite
#   no patches     every *.patch in this directory
set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || exit 1
here=test/injection/mutations

integration=0
if [[ "${1:-}" == "--integration" ]]; then integration=1; shift; fi
patches=("$@")
[[ ${#patches[@]} -eq 0 ]] && patches=("$here"/*.patch)

if ! git diff --quiet -- internal api cmd; then
	echo "product code has uncommitted changes; refusing to mutate it" >&2
	exit 1
fi

current=""
restore() { [[ -n "$current" ]] && git apply -R "$current" 2>/dev/null; current=""; }
trap 'restore; exit 130' INT TERM

printf '%-36s %-8s %s\n' MUTANT UNIT INTEGRATION
for patch in "${patches[@]}"; do
	name="$(basename "$patch" .patch)"
	git apply "$patch" || { printf '%-36s %s\n' "$name" "does-not-apply"; continue; }
	current="$patch"

	if go test -count=1 ./... >/dev/null 2>&1; then unit=survived; else unit=killed; fi

	integ=-
	if [[ $unit == survived && $integration -eq 1 ]]; then
		if go test -count=1 -tags e2e -timeout 300s ./test/integration/... ./internal/store/postgres/... >/dev/null 2>&1; then
			integ=survived
		else
			integ=killed
		fi
	fi
	restore
	printf '%-36s %-8s %s\n' "$name" "$unit" "$integ"
done

git diff --quiet -- internal api cmd || { echo "product code was left modified" >&2; exit 1; }
