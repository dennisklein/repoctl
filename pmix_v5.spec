%global _lto_cflags %nil
%global _prefix /opt/%{name}
%global _name pmix
%global _pmix_v5_pkgconfig /usr/%{_lib}/pkgconfig/%{name}.pc

Name:           pmix_v5
Version:        5.0.10
Release:        %autorelease
Summary:        Process Management Interface Exascale (PMIx)
License:        BSD-3-Clause
URL:            https://pmix.org/
Source0:        https://github.com/openpmix/openpmix/releases/download/v%{version}/%{_name}-%{version}.tar.bz2

BuildRequires:  autoconf
BuildRequires:  automake
BuildRequires:  flex
BuildRequires:  gcc
BuildRequires:  hwloc-devel
BuildRequires:  libevent-devel
BuildRequires:  libtool
BuildRequires:  make
BuildRequires:  munge-devel
BuildRequires:  perl-interpreter
BuildRequires:  python3-devel
BuildRequires:  zlib-devel

ExcludeArch:    %{ix86}

%description
The Process Management Interface (PMI) has been used for quite some time as
a means of exchanging wireup information needed for interprocess
communication. Two versions (PMI-1 and PMI-2) have been released as part of
the MPICH effort. While PMI-2 demonstrates better scaling properties than its
PMI-1 predecessor, attaining rapid launch and wireup of the roughly 1M
processes executing across 100k nodes expected for exascale operations remains
challenging.

PMI Exascale (PMIx) represents an attempt to resolve these questions by
providing an extended version of the PMI standard specifically designed to
support clusters up to and including exascale sizes. The overall objective of
the project is not to branch the existing pseudo-standard definitions - in
fact, PMIx fully supports both of the existing PMI-1 and PMI-2 APIs - but
rather to (a) augment and extend those APIs to eliminate some current
restrictions that impact scalability, and (b) provide a reference
implementation of the PMI-server that demonstrates the desired level of
scalability.

%package        devel
Summary:        Development files for %{name}
Requires:       %{name}%{?_isa} = %{version}-%{release}
Provides:       pkgconfig(pmix_v5) = %{version}
%description    devel
The %{name}-devel package contains libraries and header files for
developing applications that use %{name}.

%prep
%setup -q -n %{_name}-%{version}

# touch lexer sources to recompile them
find src -name \*.l -print -exec touch --no-create {} \;

%build
export CFLAGS="%{build_cflags} -Wno-unused-function -Wno-attributes"
%configure \
    --prefix=%{_prefix} \
    --sysconfdir=%{_sysconfdir}/%{name} \
    --disable-static \
    --disable-silent-rules \
    --disable-pmix-binaries \
    --enable-ipv6 \
    --enable-shared \
    --with-munge

%make_build

%check
%make_build check

%install
%make_install

# remove libtool archives
find %{buildroot} -name '*.la' | xargs rm -f

rm -rf %{buildroot}%{_mandir}/
rm -rf %{buildroot}%{_docdir}/%{_name}/
rm -rf %{buildroot}%{_datadir}/%{_name}/
mkdir -p %{buildroot}/usr/%{_lib}/pkgconfig
mv %{buildroot}%{_libdir}/pkgconfig/%{_name}.pc %{buildroot}%{_pmix_v5_pkgconfig}
rmdir %{buildroot}%{_libdir}/pkgconfig
sed -i 's/Name: pmix/Name: pmix_v5/g' %{buildroot}%{_pmix_v5_pkgconfig}

%ldconfig_scriptlets
%ldconfig_scriptlets devel

%files
%license LICENSE
%doc README.md
%dir %{_libdir}/%{_name}
%dir %{_sysconfdir}/%{name}
%config(noreplace) %{_sysconfdir}/%{name}/*.conf
%{_libdir}/lib%{_name}.so.2*
%{_libdir}/%{_name}/*.so

%files devel
%dir %{_includedir}/%{_name}
%{_includedir}/%{_name}/
%{_includedir}/%{_name}*.h
%{_libdir}/lib%{_name}.so
%{_pmix_v5_pkgconfig}

%changelog
%autochangelog
