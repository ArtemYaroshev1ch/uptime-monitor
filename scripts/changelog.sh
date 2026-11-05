#!/bin/sh
# Release notes in Markdown from Conventional Commits between the previous
# release tag and TAG (default: HEAD, i.e. "what the next release would
# contain"). Used by the release workflow and by `make changelog`.
#
#   sh scripts/changelog.sh            # since the latest tag
#   sh scripts/changelog.sh v0.2.0     # the notes of v0.2.0
#
# Commits are grouped by type: breaking changes ("feat!:" or a
# "BREAKING CHANGE:" footer), features, fixes, performance, everything else.
# Merge commits are skipped: the commits they bring in are listed instead.
# In GitHub Actions (GITHUB_REPOSITORY is set) a compare link is added.
#
# POSIX sh and awk only: it also runs from Git for Windows' sh.

set -eu

tag=${1:-HEAD}
if ! git rev-parse -q --verify "$tag^{commit}" >/dev/null; then
	echo "changelog: unknown revision $tag" >&2
	exit 2
fi

# The newest release tag reachable from the parent of TAG, so TAG itself
# (when it is a tag) does not count as "previous". Empty for the first release.
prev=$(git describe --tags --abbrev=0 --match 'v[0-9]*' "$tag^" 2>/dev/null || true)
if [ -n "$prev" ]; then
	range="$prev..$tag"
else
	range=$tag
fi

# One record per commit: hash, subject and body separated by \037 (unit
# separator), records ended by \036 (record separator). Commit messages do
# not contain these control characters, so no escaping is needed.
git log --no-merges --format='%h%x1f%s%x1f%b%x1e' "$range" | awk -v prev="$prev" '
BEGIN { RS = "\036"; FS = "\037" }
{
	hash = $1
	sub(/^\n+/, "", hash)
	if (hash == "") next
	subject = $2
	body = $3

	type = ""
	desc = subject
	# type(scope)!: description
	if (match(subject, /^[a-z]+(\([^)]*\))?!?: /)) {
		head = substr(subject, 1, RLENGTH - 2)
		desc = substr(subject, RLENGTH + 1)
		breaking = (substr(head, length(head)) == "!")
		if (breaking) head = substr(head, 1, length(head) - 1)
		scope = ""
		p = index(head, "(")
		if (p > 0) {
			type = substr(head, 1, p - 1)
			scope = substr(head, p + 1, length(head) - p - 1)
		} else {
			type = head
		}
		if (scope != "") desc = "**" scope ":** " desc
	} else {
		breaking = 0
	}
	if (body ~ /(^|\n)BREAKING[ -]CHANGE: /) breaking = 1

	if (breaking) section = 1
	else if (type == "feat") section = 2
	else if (type == "fix") section = 3
	else if (type == "perf") section = 4
	else { section = 5; desc = subject }

	n[section]++
	item[section, n[section]] = "- " desc " (" hash ")"
	total++
}
END {
	title[1] = "Breaking changes"
	title[2] = "Features"
	title[3] = "Bug fixes"
	title[4] = "Performance"
	title[5] = "Other changes"
	if (prev != "") printf "Changes since %s.\n", prev
	else printf "First release.\n"
	if (total == 0) { printf "\nNo changes.\n"; exit }
	for (s = 1; s <= 5; s++) {
		if (!n[s]) continue
		printf "\n### %s\n\n", title[s]
		for (i = 1; i <= n[s]; i++) print item[s, i]
	}
}
'

if [ -n "${GITHUB_REPOSITORY:-}" ] && [ -n "$prev" ] && [ "$tag" != HEAD ]; then
	printf '\n**Full diff:** %s/%s/compare/%s...%s\n' \
		"${GITHUB_SERVER_URL:-https://github.com}" "$GITHUB_REPOSITORY" "$prev" "$tag"
fi
