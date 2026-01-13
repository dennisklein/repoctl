Name: slurm-release
Version: 9
Release: 4%{?dist}
Summary: GSI Slurm Packages
License: LGPLv3
Source0: RPM-GPG-KEY-slurm
Source1: slurm.repo
BuildArch: noarch
#Requires: epel-release = 9

%description
This package contains the yum/dnf config and repo GPG key for the
Slurm package repos for the GSI Virgo Clusters. Those Slurm package
repos depend on enabled EPEL and CRB repos.

%prep

%build

%install
install -p -m 644 -D %{SOURCE0} \
  %{buildroot}%{_sysconfdir}/pki/rpm-gpg/RPM-GPG-KEY-slurm
install -p -m 644 -D -t %{buildroot}%{_sysconfdir}/yum.repos.d %{SOURCE1}

%files
%defattr(-,root,root,-)
%config(noreplace) %{_sysconfdir}/yum.repos.d/*
%{_sysconfdir}/pki/rpm-gpg/*

%changelog
* Wed Jan 21 2026 Dennis Klein <d.klein@gsi.de> - 9-4
- Update repo URLs to slurm-25.11
- Update key expiration to 2030-01-20
* Wed Jul 16  2025 Dennis Klein <d.klein@gsi.de> - 9-3
- Fix repo URL
* Fri Mar 21 2025 Dennis Klein <d.klein@gsi.de> - 9-2
- Expire metadata after one hour
* Wed Nov 27 2024 Dennis Klein <d.klein@gsi.de> - 9-1
- Release for rocky+epel-9
