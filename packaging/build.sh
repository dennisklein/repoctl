#!/bin/bash
# Builds the binary and the unsigned RPMs of a repoctl release
# (docs/design/tool.html#pages).
#
#   packaging/build.sh VERSION KEYFILE OUT
#
# VERSION is the tag, for example v0.4.0 or v0.5.0-rc.1. KEYFILE is the
# public key file of repoctl-release (packaging/RPM-GPG-KEY-repoctl in a
# release). OUT gets:
#
#   repoctl-VERSION-linux-x86_64.tar.gz   the binary, completions, LICENSE
#   repoctl-*.fcN.x86_64.rpm              the tool RPM per Fedora release
#   repoctl-release-*.noarch.rpm          the release RPM
#   repoctl-mirror-*.noarch.rpm           the mirror job RPM
#
# The binary is built once on this host with the Go of go.mod: the golang of
# a Fedora release can be older than go.mod needs. The tool RPM is made in a
# Fedora container for each Fedora release of packaging/releasevers, the
# noarch RPMs once in a Rocky Linux 9 container, the oldest EL of the repo.
#
# Environment:
#   REPOCTL_TOOL_KEYS  sets doctor.ToolKeys of the binary (the RPM test of CI)
#   CONTAINER          the container program (docker)
#   FEDORA_IMAGE       the Fedora image without the tag (quay.io/fedora/fedora)
#   EL_IMAGE           the EL image of the noarch RPMs (quay.io/rockylinux/rockylinux:9)
set -euo pipefail

if (($# != 3)); then
	echo "usage: packaging/build.sh VERSION KEYFILE OUT" >&2
	exit 2
fi
tag=$1 keyfile=$2 out=$3
root=$(git rev-parse --show-toplevel)
# shellcheck source=packaging/releasevers
. "$root/packaging/releasevers"
container=${CONTAINER:-docker}
fedora_image=${FEDORA_IMAGE:-quay.io/fedora/fedora}
el_image=${EL_IMAGE:-quay.io/rockylinux/rockylinux:9}

version=${tag#v}
rpmver=${version/-/\~}
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || {
	echo "build.sh: $tag is not vMAJOR.MINOR.PATCH[-PRE]" >&2
	exit 2
}
[[ -s $keyfile ]] || {
	echo "build.sh: the key file $keyfile is missing or empty" >&2
	exit 2
}
commit=$(git -C "$root" rev-parse --short=12 HEAD)
mkdir -p "$out"
out=$(cd "$out" && pwd)

work=$(mktemp -d)
cleanup() {
	# The containers write as root; take the files back before the removal.
	"$container" run --rm -v "$work:/work" "$el_image" chown -R "$(id -u):$(id -g)" /work >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$work/SOURCES" "$work/SPECS" "$work/out"

# The binary and its completions.
src=$work/repoctl-$rpmver
mkdir -p "$src/completions"
pkg=github.com/dennisklein/repoctl/internal
ldflags="-B gobuildid -X $pkg/config.Version=$version -X $pkg/config.Commit=$commit"
if [[ -n ${REPOCTL_TOOL_KEYS:-} ]]; then
	ldflags+=" -X $pkg/doctor.ToolKeys=$REPOCTL_TOOL_KEYS"
fi
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$ldflags" -o "$src/repoctl" ./cmd/repoctl)
"$src/repoctl" completion bash >"$src/completions/repoctl.bash"
"$src/repoctl" completion zsh >"$src/completions/_repoctl"
"$src/repoctl" completion fish >"$src/completions/repoctl.fish"
cp "$root/LICENSE" "$root/README.md" "$src/"
tar -C "$work" -czf "$work/SOURCES/repoctl-$rpmver.tar.gz" "repoctl-$rpmver"
cp "$work/SOURCES/repoctl-$rpmver.tar.gz" "$out/repoctl-$version-linux-x86_64.tar.gz"

# The sources of the noarch RPMs.
msrc=$work/repoctl-mirror-$rpmver
mkdir -p "$msrc"
cp "$root"/contrib/mirror/* "$root/LICENSE" "$msrc/"
tar -C "$work" -czf "$work/SOURCES/repoctl-mirror-$rpmver.tar.gz" "repoctl-mirror-$rpmver"
cp "$root/contrib/mirror/repoctl-mirror.sysusers" "$work/SOURCES/"
cp "$keyfile" "$work/SOURCES/RPM-GPG-KEY-repoctl"
cp "$root/packaging/repoctl.repo" "$work/SOURCES/"
cp "$root"/packaging/*.spec "$work/SPECS/"

# rpmbuild in a container: IMAGE TOPDIR SPEC...
rpmbuild_in() {
	local image=$1 top=$2
	shift 2
	local specs=()
	for s in "$@"; do specs+=("/work/SPECS/$s"); done
	# shellcheck disable=SC2016 # the script runs in the container
	"$container" run --rm -v "$work:/work" "$image" bash -euc '
		top=$1 version=$2
		shift 2
		dnf -y -q install rpm-build systemd-rpm-macros >/dev/null
		for spec; do
			rpmbuild -bb --define "_topdir /work/$top" --define "_sourcedir /work/SOURCES" \
				--define "repoctl_version $version" "$spec"
		done
		cp /work/"$top"/RPMS/*/*.rpm /work/out/
	' rpmbuild "$top" "$rpmver" "${specs[@]}"
}

for fc in $FEDORA; do
	rpmbuild_in "$fedora_image:$fc" "top-fc$fc" repoctl.spec
done
rpmbuild_in "$el_image" top-noarch repoctl-release.spec repoctl-mirror.spec

cp "$work"/out/*.rpm "$out/"
ls -l "$out"
