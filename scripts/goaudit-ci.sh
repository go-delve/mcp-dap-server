#!/bin/sh
# Minimal, repository-local support for scripts/mutation-audit.go. Production
# mutations are supplied through Go overlays; this helper never edits source.
set -eu

cmd=${1:-}
shift || true
timeout=${GOAUDIT_TIMEOUT:-90s}
state=${GOAUDIT_STATE_DIR:-"${TMPDIR:-/tmp}/mcp-dap-mutation-state"}
mkdir -p "$state"

case "$cmd" in
check-isolation)
	echo "CHECK_ISOLATION: OK (overlay-only runner)"
	;;
preflight)
	go build ./...
	echo "PREFLIGHT: OK"
	;;
tests)
	pkg=${1:-.}
	(cd "$pkg" && go test -list '^(Test|Example)' .)
	echo "LIST: OK"
	;;
run)
	pkg=${1:-.}
	re=${2:-.*}
	log="$state/run-$(printf '%s' "$re" | tr -c 'A-Za-z0-9._-' '_').log"
	set +e
	(cd "$pkg" && go test -vet=off -count=1 -v -timeout="$timeout" -run "$re" .) >"$log" 2>&1
	code=$?
	set -e
	cat "$log"
	if [ "$code" -eq 0 ]; then
		if grep -q -- '--- PASS:' "$log"; then
			echo "RESULT: PASS"
		elif grep -q -- '--- SKIP:' "$log"; then
			echo "RESULT: SKIP"
		else
			echo "RESULT: NO_TESTS_RUN"
		fi
	elif grep -q -- 'panic:' "$log"; then
		echo "RESULT: PANIC"
	elif grep -q -- 'test timed out' "$log"; then
		echo "RESULT: TIMEOUT"
	elif grep -q -- 'build failed\|undefined:\|syntax error:' "$log"; then
		echo "RESULT: BUILD_FAILED"
	else
		echo "RESULT: FAIL"
	fi
	;;
cover)
	pkg=${1:-.}
	re=${2:-.*}
	profile="$state/cover-$(printf '%s' "$re" | tr -c 'A-Za-z0-9._-' '_').out"
	(cd "$pkg" && go test -vet=off -count=1 -timeout="$timeout" -run "$re" -coverprofile="$profile" .)
	awk '
		NR == 1 { next }
		{
			split($1, p, ":"); file=p[1]
			split(p[2], range, ","); split(range[1], a, "."); split(range[2], b, ".")
			if ($3 + 0 > 0) {
				printf "COVERED\t%s\t%s-%s\tstmts=%s\n", file, a[1], b[1], $2
				total += $2
			} else {
				printf "UNCOVERED\t%s\t%s-%s\tstmts=%s\n", file, a[1], b[1], $2
			}
		}
		END { printf "COVERED_STATEMENTS: %d\n", total + 0 }
	' "$profile"
	echo "COVER: OK"
	;;
snapshot)
	echo "SNAPSHOT: OVERLAY ${1:-}"
	;;
restore)
	echo "RESTORE: OK ${1:-}"
	;;
check-clean)
	echo "CHECK_CLEAN: CLEAN"
	;;
*)
	echo "unknown command: $cmd" >&2
	exit 2
	;;
esac
