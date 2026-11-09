#!/bin/bash
# Imports the tool repository key for the release job and the yank job
# (docs/design/tool.html#tool-key) and prints its fingerprint.
#
#   TOOL_REPO_KEY=… GNUPGHOME=DIR packaging/import-key.sh
#
# TOOL_REPO_KEY is the armored secret key without a passphrase: the secret
# of the GitHub environment tool-release. The script stops when the key is
# not "current" in the key list of the binary (internal/doctor ToolKeys) or
# not in packaging/RPM-GPG-KEY-repoctl, the key file of repoctl-release.
set -euo pipefail

: "${TOOL_REPO_KEY:?TOOL_REPO_KEY is empty: the job must run in the environment tool-release}"
: "${GNUPGHOME:?GNUPGHOME is not set}"
root=$(git rev-parse --show-toplevel)
keyfile=$root/packaging/RPM-GPG-KEY-repoctl

mkdir -p "$GNUPGHOME"
chmod 0700 "$GNUPGHOME"
printf '%s\n' "$TOOL_REPO_KEY" | gpg --batch --quiet --import 2>/dev/null
mapfile -t fprs < <(gpg --batch --list-secret-keys --with-colons | awk -F: '$1 == "fpr" { print $10 }')
if ((${#fprs[@]} == 0)); then
	echo "import-key.sh: TOOL_REPO_KEY holds no secret key" >&2
	exit 1
fi
fpr=${fprs[0]}
if ! (cd "$root" && go run ./packaging/rpmtree keys) | grep -q "^$fpr current "; then
	echo "import-key.sh: the key $fpr is not current in ToolKeys (internal/doctor/toolkey.go)" >&2
	exit 1
fi
if ! gpg --batch --show-keys --with-colons "$keyfile" | grep -q "^fpr:::::::::$fpr:"; then
	echo "import-key.sh: the key $fpr is not in $keyfile" >&2
	exit 1
fi
echo "$fpr"
