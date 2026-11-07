# The mirror job RPM of repoctl (docs/design/tool.html#pages,
# publish.html#mirror) for the HTTP server. The release job builds it once,
# in a rockylinux:9 container, for Fedora and EL.
Name:           repoctl-mirror
Version:        %{repoctl_version}
Release:        1
Summary:        The repoctl mirror job for the HTTP server
License:        Apache-2.0
URL:            https://github.com/dennisklein/repoctl
Source0:        repoctl-mirror-%{version}.tar.gz
# The sysusers file again: the compat macro of %%pre reads it when rpmbuild
# parses the spec.
Source1:        repoctl-mirror.sysusers
BuildArch:      noarch

BuildRequires:  systemd-rpm-macros
Requires:       bash
Requires:       coreutils
Requires:       curl
Requires:       jq
Requires:       gnupg2
Requires:       rpm
Requires:       gzip
Requires:       util-linux
%{?systemd_requires}
%{?sysusers_requires_compat}

%description
The mirror job serves the snapshot latest of each set and target of repoctl
on the HTTP server. A systemd timer runs it every 5 minutes. It verifies each
new snapshot with the keys of the clients before it swaps the served tree.

%prep
%setup -q

%build

%install
install -D -m 0755 mirror %{buildroot}%{_libexecdir}/repoctl-mirror/mirror
install -D -m 0644 repoctl-mirror.sysusers %{buildroot}%{_sysusersdir}/repoctl-mirror.conf
install -D -m 0644 repoctl-mirror.tmpfiles %{buildroot}%{_tmpfilesdir}/repoctl-mirror.conf
install -D -m 0644 repoctl-mirror.service %{buildroot}%{_unitdir}/repoctl-mirror.service
install -D -m 0644 repoctl-mirror-failed.service %{buildroot}%{_unitdir}/repoctl-mirror-failed.service
install -D -m 0644 repoctl-mirror.timer %{buildroot}%{_unitdir}/repoctl-mirror.timer
install -D -m 0644 repoctl-mirror.conf %{buildroot}%{_sysconfdir}/repoctl-mirror.conf

%pre
%sysusers_create_compat %{SOURCE1}

%post
%systemd_post repoctl-mirror.timer
%tmpfiles_create %{_tmpfilesdir}/repoctl-mirror.conf

%preun
%systemd_preun repoctl-mirror.timer repoctl-mirror.service

# The timer starts the new job at its next run; no restart.
%postun
%systemd_postun repoctl-mirror.timer

%files
%license LICENSE
%doc README
%{_libexecdir}/repoctl-mirror/
%{_sysusersdir}/repoctl-mirror.conf
%{_tmpfilesdir}/repoctl-mirror.conf
%{_unitdir}/repoctl-mirror.service
%{_unitdir}/repoctl-mirror-failed.service
%{_unitdir}/repoctl-mirror.timer
%config(noreplace) %{_sysconfdir}/repoctl-mirror.conf
