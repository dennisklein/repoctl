%global tag v%{version}
%global name_tag %{name}-v%{version}

Name:           slurm-singularity-exec
Version:        1.0.0
Release:        %autorelease
Summary:        Slurm SPANK plugin to start Singularity/Apptainer containers

License:        GPLv3
Source0:        https://git.gsi.de/SDE/%{name}/-/archive/%{tag}/%{name_tag}.tar.bz2

BuildRequires:  gcc
BuildRequires:  gcc-c++
BuildRequires:  libstdc++-static
BuildRequires:  make
BuildRequires:  slurm-devel

Requires:       apptainer
Requires:       slurm-slurmd

%description
The Singularity SPANK plug-in provides the users with an interface to launch an
application within a Singularity container. The plug-in adds multiple
command-line options to the salloc, srun and sbatch commands. These options are
then propagated to a shell script slurm-singularity-wrapper.sh customizable by
the cluster administrator.

%prep
%autosetup -n %{name_tag}

%build
make

%clean
rm -rf %{buildroot}

%install
install -p -m 0644 -D singularity-exec.so \
   %{buildroot}/%{_libdir}/slurm/singularity-exec.so
install -p -m 0644 -D slurm-singularity-wrapper.sh \
   %{buildroot}/%{_libexecdir}/slurm-singularity-wrapper.sh
install -p -m 0644 -D singularity-exec.conf \
   %{buildroot}/%{_docdir}/%{name}/singularity-exec.conf

%files
%{_libdir}/slurm/singularity-exec.so
%{_libexecdir}/slurm-singularity-wrapper.sh
%{_docdir}/%{name}/singularity-exec.conf
%license LICENSE
%doc README.md

%changelog
%autochangelog
