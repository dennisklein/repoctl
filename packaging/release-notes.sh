#!/bin/bash
# Writes the notes of a GitHub Release of repoctl to stdout. The release job
# runs it (docs/design/tool.html#pages). The changes come from CHANGELOG.md;
# the install and upgrade steps stay in the user manual.
#
#   release-notes.sh check TAG
#   release-notes.sh changes TAG
#   release-notes.sh notes TAG RELEASE_RPM KEYS SIGNER
#
# check    exits 1 when CHANGELOG.md has no entry for TAG.
# changes  prints the section of CHANGELOG.md for TAG, without its heading.
# notes    prints the full notes. RELEASE_RPM is the signed repoctl-release
#          RPM. KEYS is a file with one tool repository key per line:
#          FINGERPRINT STATE EXPIRES (the output of "rpmtree keys"). SIGNER
#          is the fingerprint of the key that signs this release.
#
# A release tag vX.Y.Z takes the section "## [X.Y.Z]". A pre-release tag
# vX.Y.Z-rc.N takes that section, else the section "## [Unreleased]".
#
# REPO (default: $GITHUB_REPOSITORY or dennisklein/repoctl) and PAGES
# (default: https://dennisklein.github.io/repoctl) give the links.
set -euo pipefail

REPO=${REPO:-${GITHUB_REPOSITORY:-dennisklein/repoctl}}
PAGES=${PAGES:-https://dennisklein.github.io/repoctl}
CHANGELOG=${CHANGELOG:-CHANGELOG.md}

usage() {
	echo "usage: release-notes.sh check|changes TAG" >&2
	echo "       release-notes.sh notes TAG RELEASE_RPM KEYS SIGNER" >&2
	exit 2
}

# section NAME prints the body of the section "## [NAME]" of CHANGELOG.md:
# no heading, no link references, no blank lines at the start and the end.
section() {
	awk -v h="## [$1]" '
		index($0, h) == 1 { on = 1; next }
		on && /^## / { exit }
		on && !/^\[[^]]*\]: / { print }
	' "$CHANGELOG" | sed -e '/./,$!d' | sed -e ':a' -e '/^\n*$/{$d;N;ba' -e '}'
}

# changes TAG prints the section of TAG. It fails when the section is
# missing or has no entry.
changes() {
	local tag=$1 v body
	v=${tag#v}
	body=$(section "${v%%-*}")
	if [ -z "$body" ] && [ "$v" != "${v%%-*}" ]; then
		body=$(section Unreleased)
	fi
	if ! grep -q '^- ' <<<"$body"; then
		echo "release-notes.sh: $CHANGELOG has no entry for $tag: add the section \"## [${v%%-*}] - YYYY-MM-DD\"" >&2
		return 1
	fi
	printf '%s\n' "$body"
}

# previous TAG prints the newest release tag (no pre-release) before TAG,
# or nothing.
previous() {
	local ref=$1
	git rev-parse -q --verify "$ref^{commit}" >/dev/null 2>&1 || ref=HEAD
	git tag --list 'v[0-9]*' --merged "$ref^" --sort=-v:refname 2>/dev/null |
		grep -v -e - -e "^$1\$" | head -n 1 || true
}

notes() {
	local tag=$1 rpm=$2 keys=$3 signer=$4 v rel sum prev signed=""
	v=${tag#v}
	rel=$(basename "$rpm")
	sum=$(sha256sum "$rpm" | cut -d' ' -f1)
	prev=$(previous "$tag")

	echo "repoctl $v builds signed RPM repositories from a package set. Read the [user manual]($PAGES/manual/) to start."
	echo
	echo "## Changes"
	echo
	changes "$tag" | sed -e 's/^### /#### /'
	echo
	if [ -n "$prev" ]; then
		echo "All commits: [$prev...$tag](https://github.com/$REPO/compare/$prev...$tag)."
	else
		echo "All commits: [$tag](https://github.com/$REPO/commits/$tag)."
	fi
	echo
	echo "## Install or upgrade"
	echo
	echo "| Host | Steps |"
	echo "|---|---|"
	echo "| New admin host | [First install]($PAGES/manual/install.html#first-install), with the check values below |"
	echo "| Admin host with repoctl | [Upgrade]($PAGES/manual/install.html#upgrade) |"
	echo "| HTTP server | [Mirror job]($PAGES/manual/install.html#mirror): \`sudo dnf install repoctl-mirror\` or \`sudo dnf upgrade repoctl-mirror\` |"
	echo
	echo "Check values for the first install:"
	echo
	echo "| Check | Value |"
	echo "|---|---|"
	echo "| sha256 of [\`$rel\`]($PAGES/rpm/$rel) | \`$sum\` |"
	while read -r fpr state expires; do
		[ -n "$fpr" ] || continue
		local what="Tool repository key, $state, expires $expires"
		if [ "$fpr" = "$signer" ]; then
			what="$what, signs this release"
			signed=1
		fi
		echo "| $what | \`$fpr\` |"
	done <"$keys"
	if [ -z "$signed" ]; then
		echo "| Key that signs this release | \`$signer\` |"
	fi
	echo
	echo "Verify a file of this release: \`gh attestation verify FILE --repo $REPO\`."
}

[ $# -ge 2 ] || usage
case $1 in
check) [ $# -eq 2 ] || usage; changes "$2" >/dev/null ;;
changes) [ $# -eq 2 ] || usage; changes "$2" ;;
notes) [ $# -eq 5 ] || usage; notes "$2" "$3" "$4" "$5" ;;
*) usage ;;
esac
