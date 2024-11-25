%global plugstack_conf_d /slurm/plugstack.conf.d

Name:           slurm-singularity-exec
Version:        3.1.0
Release:        %autorelease
Epoch:          1
Summary:        Slurm SPANK plugin to start Singularity/Apptainer containers

License:        GPLv3
Source0:        https://github.com/GSI-HPC/%{name}/archive/refs/tags/v%{version}.tar.gz

BuildRequires:  cmake
BuildRequires:  gcc
BuildRequires:  gcc-c++
BuildRequires:  libstdc++-static
BuildRequires:  slurm-devel = 24.05.4

Requires:       apptainer
Requires:       slurm-slurmd = 24.05.4

%description
The Singularity SPANK plug-in provides the users with an interface to launch an
application within a Singularity container. The plug-in adds multiple
command-line options to the salloc, srun and sbatch commands. These options are
then propagated to a shell script slurm-singularity-wrapper.sh customizable by
the cluster administrator.

%prep
%autosetup

%build
%cmake -D INSTALL_PLUGSTACK_CONF=ON
%cmake_build

%install
%cmake_install
install -d -m 0755 %{buildroot}%{_datadir}%{plugstack_conf_d}
mv %{buildroot}%{_sysconfdir}%{plugstack_conf_d}/singularity-exec.conf \
    %{buildroot}%{_datadir}%{plugstack_conf_d}
rm -rf %{buildroot}%{_sysconfdir}%{plugstack_conf_d}

%files
%{_libexecdir}/slurm-singularity-exec.so
%{_libexecdir}/slurm-singularity-wrapper.sh
%config(noreplace) %{_datadir}%{plugstack_conf_d}/singularity-exec.conf
%license LICENSE
%doc README.md

%changelog
%autochangelog
