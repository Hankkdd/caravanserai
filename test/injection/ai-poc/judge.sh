#!/usr/bin/env bash
# Judge one AI-written test without trusting the agent's own report.
#
#   AGENT_ROOT=<folder holding one copy per mutant> test/injection/ai-poc/judge.sh <mutant-name>
#
# AGENT_ROOT/<mutant-name> is the folder the agent worked in: a pristine copy of
# the baseline plus mutant.patch. WORK_ROOT (default: a folder under TMPDIR)
# holds the clean trees the test is copied into.
set -uo pipefail
export LC_ALL=C
m="$1"
SRC="$(git rev-parse --show-toplevel)"
: "${AGENT_ROOT:?set AGENT_ROOT to the folder holding the agent copies}"
AGENT=$AGENT_ROOT/$m
WORK_ROOT=${WORK_ROOT:-${TMPDIR:-/tmp}/cara-ai-judge}
WORK=$WORK_ROOT/$m
PRISTINE=$WORK_ROOT/pristine

[ -d "$PRISTINE" ] || { mkdir -p "$PRISTINE"; git -C "$SRC" archive 2c88557 | tar -x -C "$PRISTINE"; }

echo "== $m =="
# 1. Rule compliance: what did the agent change in its folder?
changes="$(diff -rq "$PRISTINE" "$AGENT" 2>/dev/null | grep -v 'mutant.patch')"
added="$(echo "$changes" | grep "^Only in $AGENT" | sed -E "s#^Only in ([^:]+): (.*)#\1/\2#")"
modified="$(echo "$changes" | grep '^Files ' )"
echo "新增：${added:-（無）}" | sed "s#$AGENT/##g"
[ -n "$modified" ] && echo "違規：修改了既有檔案" && echo "$modified" | sed "s#$AGENT/##g; s#$PRISTINE/##g"
cmp -s "$SRC/test/injection/mutations/$m.patch" "$AGENT/mutant.patch" || echo "違規：mutant.patch 被改過"

tests="$(find "$AGENT" -name '*_ai_test.go' | sed "s#$AGENT/##")"
n=$(echo "$tests" | grep -c . )
[ "$n" -eq 1 ] || { echo "結果：不合格（找到 $n 個 _ai_test.go）"; exit 0; }
echo "測試檔：$tests"
grep -nE 'os\.ReadFile|ioutil\.ReadFile|go/parser|mutant\.patch|\.go"' "$AGENT/$tests" && echo "可疑：測試在讀原始碼"

# 2. Copy only the new test into a fresh pristine tree.
rm -rf -- "${WORK:?}"; mkdir -p "$WORK"; cp -a "$PRISTINE/." "$WORK/"
cp "$AGENT/$tests" "$WORK/$tests"
pkg="./$(dirname "$tests")/"
tags=""; head -1 "$WORK/$tests" | grep -q 'go:build e2e' && tags="-tags e2e"
cd "$WORK" || exit 1
names="$(grep -oE '^func (Test[A-Za-z0-9_]+)' "$tests" | awk '{print $2}' | paste -sd'|')"
[ -n "$names" ] || { echo "結果：不合格（檔案裡沒有 Test 函式）"; exit 0; }
echo "測試函式：$names"

# 3. Clean must pass, five times in a row.
if go test $tags -count=5 -timeout 600s "$pkg" -run "^($names)\$" >"$WORK_ROOT/$m.clean.log" 2>&1; then clean=PASS; else clean=FAIL; fi
# 4. Mutant must fail.
patch -p1 -s < "$SRC/test/injection/mutations/$m.patch"
if go test $tags -count=1 -timeout 600s "$pkg" -run "^($names)\$" >"$WORK_ROOT/$m.mutant.log" 2>&1; then mut=PASS; else mut=FAIL; fi
patch -R -p1 -s < "$SRC/test/injection/mutations/$m.patch"

echo "乾淨 cara（連跑 5 次）：$clean    套用變異體：$mut"
if [ "$clean" = PASS ] && [ "$mut" = FAIL ]; then echo "結果：✓ 殺掉變異體"; else echo "結果：✗ 沒有殺掉"; fi
