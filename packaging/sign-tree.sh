#!/bin/bash
# Signs the new RPMs and makes the signed RPM repo of repoctl
# (docs/design/tool.html#pages).
#
#   packaging/sign-tree.sh SIGNER KEYFILE DIST OLD OUT
#
# SIGNER is the fingerprint of the tool repository key; its secret key is in
# GNUPGHOME. KEYFILE is the public key file that the clients have. DIST holds
# the unsigned new RPMs (empty for the yank job). OLD is the root of the old
# tree (with rpm/; empty or missing for the first release). The script
#
#   1. signs each RPM of DIST with rpmsign,
#   2. assembles OUT/rpm/ with packaging/rpmtree (packaging/yanked.txt,
#      packaging/releasevers),
#   3. runs createrepo_c in each directory and signs each repomd.xml,
#   4. checks each RPM with rpmkeys and each repomd.xml.asc with gpgv
#      against KEYFILE,
#   5. writes OUT/rpm-repo.tar and OUT/added.txt (the new RPMs of the tree).
#
# It needs rpmsign, rpmkeys, createrepo_c, gpg, gpgv and go.
set -euo pipefail

if (($# != 5)); then
	echo "usage: packaging/sign-tree.sh SIGNER KEYFILE DIST OLD OUT" >&2
	exit 2
fi
signer=$1 keyfile=$2 dist=$3 old=$4 out=$5
root=$(git rev-parse --show-toplevel)
# shellcheck source=packaging/releasevers
. "$root/packaging/releasevers"
: "${GNUPGHOME:?GNUPGHOME must hold the secret tool repository key}"

# 1. Sign. The tree takes only the files that it does not have yet.
shopt -s nullglob
rpms=("$dist"/*.rpm)
if ((${#rpms[@]} > 0)); then
	rpmsign --addsign --define "_gpg_name $signer" --define "_gpg_path $GNUPGHOME" \
		--define "_gpg_digest_algo sha256" "${rpms[@]}" >/dev/null
fi

# 2. Assemble.
mkdir -p "$out"
(cd "$root" && go run ./packaging/rpmtree assemble -old "$old" -new "$dist" -out "$out/tree" \
	-yanked packaging/yanked.txt -fedora "$FEDORA" -el "$EL") >"$out/added.txt"

# 3. Repodata and its signature.
for d in "$out"/tree/rpm/*/x86_64; do
	createrepo_c --quiet --no-database --general-compress-type=gz "$d"
	gpg --batch --yes --local-user "$signer" --detach-sign --armor \
		--output "$d/repodata/repomd.xml.asc" "$d/repodata/repomd.xml"
done

# 4. Verify as a client does: only the key of KEYFILE counts.
check=$(mktemp -d)
trap 'rm -rf "$check"' EXIT
rpm --dbpath "$check/rpmdb" --initdb
rpmkeys --dbpath "$check/rpmdb" --import "$keyfile"
gpg --batch --yes --dearmor --output "$check/keyring.gpg" "$keyfile"
for f in "$out"/tree/rpm/*/x86_64/*.rpm "$out"/tree/rpm/*.rpm; do
	res=$(rpmkeys --dbpath "$check/rpmdb" --checksig -- "$f")
	[[ $res == *": digests signatures OK" ]] || {
		echo "sign-tree.sh: $res" >&2
		exit 1
	}
done
for d in "$out"/tree/rpm/*/x86_64; do
	gpgv --quiet --keyring "$check/keyring.gpg" "$d/repodata/repomd.xml.asc" "$d/repodata/repomd.xml"
done

# 5. The tar of the tree for the GitHub Release and the Pages job.
tar -C "$out/tree" -cf "$out/rpm-repo.tar" rpm
echo "sign-tree.sh: the tree has $(find "$out/tree/rpm" -name '*.rpm' | wc -l) RPM files; new:"
sed 's/^/  /' "$out/added.txt"
