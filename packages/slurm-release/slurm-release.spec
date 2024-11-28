Name: slurm-release
Version: 9
Release: 1%{?dist}
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
* Wed Nov 27 2024 Dennis Klein <d.klein@gsi.de> - 9-1
- Release for rocky+epel-9
