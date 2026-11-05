#!/bin/sh
# Fails if any of the given packages has less statement coverage than MIN
# percent (PROMPT.md §13: internal/service and internal/checker >= 70 %).
#
#   sh scripts/cover-check.sh MIN PACKAGE...
#   sh scripts/cover-check.sh 70 ./internal/service ./internal/checker
#
# A package must be listed exactly (not with /...): every listed package has
# to have tests, otherwise it counts as a failure.
#
# POSIX sh only: it also runs from Git for Windows' sh.

set -eu

if [ "$#" -lt 2 ]; then
	echo "usage: sh scripts/cover-check.sh MIN PACKAGE..." >&2
	exit 2
fi
min=$1
shift

# The output is kept in a variable instead of piped: in a pipe, sh would
# report the exit status of awk and hide a failing test.
out=$(go test -count=1 -cover "$@") || {
	printf '%s\n' "$out"
	exit 1
}

# Lines look like:
#   ok   <pkg>  0.5s  coverage: 96.6% of statements
#   ?    <pkg>  [no test files]
printf '%s\n' "$out" | awk -v min="$min" '
	$1 == "?" { printf "FAIL  %s: no test files\n", $2; bad = 1; next }
	/coverage:/ {
		for (i = 1; i <= NF; i++) if ($i == "coverage:") pct = $(i + 1)
		sub("%", "", pct)
		ok = (pct + 0 >= min)
		printf "%s  %s: %s%% (minimum %s%%)\n", (ok ? "ok  " : "FAIL"), $2, pct, min
		if (!ok) bad = 1
	}
	END { exit bad }
'
