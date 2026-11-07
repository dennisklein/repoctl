# The release RPM of repoctl (docs/design/tool.html#pages): the public tool
# repository key and the repo file. Its version is its own: raise it with
# each change of RPM-GPG-KEY-repoctl or repoctl.repo, and change the sha256
# of the first install (tool.html#tool-key) in the same commit. A version
# that the RPM repo already has keeps its released file.
Name:           repoctl-release
Version:        1
Release:        1
Summary:        The repo file and the key of the repoctl RPM repo
License:        Apache-2.0
URL:            https://github.com/dennisklein/repoctl
Source0:        RPM-GPG-KEY-repoctl
Source1:        repoctl.repo
BuildArch:      noarch

%description
The repo file /etc/yum.repos.d/repoctl.repo of the RPM repo of repoctl on
GitHub Pages, and the public tool repository key that signs its RPMs and its
repodata.

%prep

%build

%install
install -D -m 0644 %{SOURCE0} %{buildroot}%{_sysconfdir}/pki/rpm-gpg/RPM-GPG-KEY-repoctl
install -D -m 0644 %{SOURCE1} %{buildroot}%{_sysconfdir}/yum.repos.d/repoctl.repo

%files
%{_sysconfdir}/pki/rpm-gpg/RPM-GPG-KEY-repoctl
# Not noreplace: an upgrade or a reinstall restores gpgcheck and
# repo_gpgcheck (doctor check tool.repo-key).
%config %{_sysconfdir}/yum.repos.d/repoctl.repo
