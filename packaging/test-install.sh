#!/bin/bash
# Installs repoctl from a signed RPM repo as the first install does
# (docs/design/tool.html#tool-bootstrap), in a Fedora or EL container.
#
#   packaging/test-install.sh TREE PACKAGE...
#
# TREE is the root of the tree (with rpm/). The script installs the release
# RPM from TREE/rpm/, points repoctl.repo at TREE (gpgcheck and repo_gpgcheck
# stay on), installs each PACKAGE with dnf and checks it. CI runs it in each
# release of packaging/releasevers.
set -euo pipefail

tree=$1
shift
rv=$(rpm -E '%{?fedora}%{?rhel}')
echo "== $(. /etc/os-release && echo "$PRETTY_NAME"), releasever $rv"

dnf -y -q install "$tree"/rpm/repoctl-release-*.noarch.rpm
# dnf expands $releasever and $basearch as for the Pages repo.
sed -i "s|^baseurl=.*|baseurl=file://$tree/rpm/\$releasever/\$basearch/|" /etc/yum.repos.d/repoctl.repo
grep -qx 'gpgcheck=1' /etc/yum.repos.d/repoctl.repo
grep -qx 'repo_gpgcheck=1' /etc/yum.repos.d/repoctl.repo

for pkg; do
	dnf -y -q --setopt=install_weak_deps=False install "$pkg"
	rpm -q "$pkg"
	rpm -V "$pkg"
	case $pkg in
	repoctl)
		repoctl version
		dnf -y -q install jq
		# Other host checks fail in a container (no mock group); only
		# tool.repo-key counts here.
		check=$( (repoctl doctor --only host --json || true) | jq -c '.items[0].checks[] | select(.name == "tool.repo-key")')
		echo "$check"
		[[ $(jq -r .status <<<"$check") == ok ]]
		[[ $(jq -r .detail <<<"$check") == *"has the tool repository key"* ]]
		;;
	repoctl-mirror)
		getent passwd repoctl-mirror
		test -x /usr/libexec/repoctl-mirror/mirror
		bash -n /usr/libexec/repoctl-mirror/mirror
		test -f /etc/repoctl-mirror.conf
		test -f /usr/lib/systemd/system/repoctl-mirror.timer
		;;
	esac
done
echo "== ok: $*"
