%{!?configure_options: %global configure_options %{nil}}
%bcond_without cma
%bcond_with    cuda
%bcond_with    gdrcopy
%bcond_without ib
%bcond_with    knem
%bcond_without rdmacm
%bcond_with    rocm
%bcond_with    ugni
%bcond_with    xpmem
%bcond_with    vfs
%bcond_without mlx5
%bcond_with    efa
%bcond_without mad

Name: ucx
Version: 1.20.0
Release: %autorelease
Summary: UCX is a communication library implementing high-performance messaging

License: BSD-3-Clause AND MIT AND CC-PDDC AND (BSD-3-Clause OR Apache-2.0)
# CC-PDDC
# src/ucm/ptmalloc286/malloc-2.8.6.h
# src/ucm/ptmalloc286/malloc.c
# MIT
# src/ucs/datastruct/khash.h
# BSD-3-Clause or Apache-2.0
# src/ucs/arch/aarch64/memcpy_thunderx2.S
# BSD-3-Clause
# All other files

URL: http://www.openucx.org
Source: https://github.com/openucx/%{name}/releases/download/%{version}/ucx-%{version}.tar.gz

# UCX currently supports only the following architectures
ExclusiveArch: aarch64 ppc64le x86_64 riscv64

%if %{defined extra_deps}
Requires: %{?extra_deps}
%endif

BuildRequires: automake autoconf libtool gcc-c++
%if "%{_vendor}" == "suse"
BuildRequires: libnuma-devel
%else
BuildRequires: numactl-devel
%endif
%if %{with cma}
BuildRequires: glibc-devel >= 2.15
%endif
%if %{with gdrcopy}
BuildRequires: gdrcopy
%endif
%if %{with ib}
BuildRequires: libibverbs-devel
%endif
%if %{with mlx5}
BuildRequires: rdma-core-devel
%endif
%if %{with mad}
BuildRequires: libibmad-devel libibumad-devel
%endif
%if %{with knem}
BuildRequires: knem
%endif
%if %{with rdmacm}
BuildRequires: librdmacm-devel
%endif
%if %{with rocm}
BuildRequires: hsa-rocr-dev
%endif
%if %{with xpmem}
BuildRequires: xpmem-devel
%endif
%if %{with vfs}
BuildRequires: fuse3-devel
%endif

%description
UCX is an optimized communication framework for high-performance distributed
applications. UCX utilizes high-speed networks, such as RDMA (InfiniBand, RoCE,
etc), Cray Gemini or Aries, for inter-node communication. If no such network is
available, TCP is used instead. UCX supports efficient transfer of data in
either main memory (RAM) or GPU memory (through CUDA and ROCm libraries). In
addition, UCX provides efficient intra-node communication, by leveraging the
following shared memory mechanisms: posix, sysv, cma, knem, and xpmem.
The acronym UCX stands for "Unified Communication X".


%if "%{_vendor}" == "suse"
%debug_package
%endif

%package devel
Requires: %{name}%{?_isa} = %{version}-%{release}
Summary: Header files required for developing with UCX

%description devel
Provides header files and examples for developing with UCX.

%prep
%setup -q
autoreconf -fiv

%build

%define _with_arg()   %{expand:%%{?with_%{1}:--with-%{2}}%%{!?with_%{1}:--without-%{2}}}
%define _enable_arg() %{expand:%%{?with_%{1}:--enable-%{2}}%%{!?with_%{1}:--disable-%{2}}}
%configure --disable-optimizations \
           --disable-logging \
           --disable-debug \
           --disable-assertions \
           --disable-params-check \
           --enable-mt \
           --without-java \
           --without-go \
           %_enable_arg cma cma \
           %_with_arg cuda cuda \
           %_with_arg gdrcopy gdrcopy \
           %_with_arg ib verbs \
           %_with_arg mlx5 mlx5 \
           %_with_arg efa efa \
           %_with_arg mad mad \
           %_with_arg knem knem \
           %_with_arg rdmacm rdmacm \
           %_with_arg rocm rocm \
           %_with_arg xpmem xpmem \
           %_with_arg vfs fuse3 \
           %_with_arg ugni ugni \
           %{?configure_options}
make %{?_smp_mflags} V=1

%install
make DESTDIR=%{buildroot} install
rm -f %{buildroot}%{_libdir}/*.la
rm -f %{buildroot}%{_libdir}/*.a
rm -f %{buildroot}%{_libdir}/ucx/*.la
rm -f %{buildroot}%{_libdir}/ucx/lib*.so
rm -f %{buildroot}%{_libdir}/ucx/lib*.a

%files
%{_libdir}/lib*.so.*
%{_bindir}/ucx_info
%{_bindir}/ucx_perftest
%{_bindir}/ucx_perftest_daemon
%{_bindir}/ucx_read_profile
%{_bindir}/io_demo
%{_datadir}/ucx
%dir %{_sysconfdir}/ucx
%{_sysconfdir}/ucx/ucx.conf
%exclude %{_datadir}/ucx/examples
%doc README AUTHORS NEWS
%{!?_licensedir:%global license %%doc}
%license LICENSE

%files devel
%{_includedir}/uc*
%{_libdir}/lib*.so
%{_libdir}/pkgconfig/ucx*.pc
%dir %{_libdir}/cmake/ucx
%{_libdir}/cmake/ucx/*.cmake
%{_datadir}/ucx/examples


%if %{with cma}
%package cma
Requires: %{name}%{?_isa} = %{version}-%{release}
Summary: UCX CMA support

%description cma
Provides CMA (Linux cross-memory-attach) transport for UCX. It utilizes the
system calls process_vm_readv/writev() for one-shot memory copy from another
process.

%files cma
%dir %{_libdir}/ucx
%{_libdir}/ucx/libuct_cma.so.*
%endif

%if %{with cuda}
%package cuda
Requires: %{name}%{?_isa} = %{version}-%{release}
Summary: UCX CUDA support

%description cuda
Provide CUDA (NVIDIA GPU) support for UCX. Enables passing GPU memory pointers
to UCX communication routines, and transports taking advantage of GPU-Direct
technology for direct data transfer between GPU and RDMA devices.

%files cuda
%dir %{_libdir}/ucx
%{_libdir}/ucx/libucx_perftest_cuda.so.*
%{_libdir}/ucx/libucm_cuda.so.*
%{_libdir}/ucx/libuct_cuda.so.*
%endif

%if %{with gdrcopy}
%package gdrcopy
Requires: %{name}-cuda%{?_isa} = %{version}-%{release}
Summary: UCX GDRCopy support

%description gdrcopy
Provide GDRCopy support for UCX. GDRCopy is a low-latency GPU memory copy
library, built on top of the NVIDIA GPUDirect RDMA technology.

%files gdrcopy
%dir %{_libdir}/ucx
%{_libdir}/ucx/libuct_cuda_gdrcopy.so.*
%endif

%if %{with ib}
%package ib
Requires: %{name}%{?_isa} = %{version}-%{release}
Summary: UCX RDMA support

%description ib
Provides support for IBTA-compliant transports for UCX. This includes RoCE,
InfiniBand, OmniPath, and any other transport supported by IB Verbs API.
Typically these transports provide RDMA support, which enables a fast and
hardware-offloaded data transfer.

%files ib
%dir %{_libdir}/ucx
%{_libdir}/ucx/libuct_ib.so.*
%endif

%if %{with mlx5}
%package ib-mlx5
Requires: %{name}%{?_isa} = %{version}-%{release}
Requires: %{name}-ib%{?_isa} = %{version}-%{release}
Summary: UCX IB MLX5 RDMA provider support

%description ib-mlx5
Provides support for DevX, Direct Verbs and DC transports for Mellanox
InfiniBand devices.

%files ib-mlx5
%dir %{_libdir}/ucx
%{_libdir}/ucx/libuct_ib_mlx5.so.*
%endif

%if %{with efa}
%package ib-efa
Requires: %{name}%{?_isa} = %{version}-%{release}
Requires: %{name}-ib%{?_isa} = %{version}-%{release}
Summary: UCX EFA device RDMA support

%description ib-efa
Provides support for AWS Elastic Fabric Adapter (EFA) device as an IBTA
transport for UCX.

%files ib-efa
%dir %{_libdir}/ucx
%{_libdir}/ucx/libuct_ib_efa.so.*
%endif

%if %{with mad}
%package mad
Requires: %{name}%{?_isa} = %{version}-%{release}
Summary: UCX InfiniBand MAD support

%description mad
Provides InfiniBand MAD support for UCX. Enables running perftest using
InfiniBand datagrams for out-of-band communications.

%files mad
%dir %{_libdir}/ucx
%{_libdir}/ucx/libucx_perftest_mad.so.*
%endif

%if %{with knem}
%package knem
Requires: %{name}%{?_isa} = %{version}-%{release}
Summary: UCX KNEM transport support

%description knem
Provides KNEM (fast inter-process copy) transport for UCX. KNEM is a Linux
kernel module that enables high-performance intra-node MPI communication
for large messages.

%files knem
%dir %{_libdir}/ucx
%{_libdir}/ucx/libuct_knem.so.*
%endif

%if %{with rdmacm}
%package rdmacm
Requires: %{name}-ib%{?_isa} = %{version}-%{release}
Summary: UCX RDMA connection manager support

%description rdmacm
Provides RDMA connection-manager support to UCX, which enables client/server
based connection establishment for RDMA-capable transports.

%files rdmacm
%dir %{_libdir}/ucx
%{_libdir}/ucx/libuct_rdmacm.so.*
%endif

%if %{with rocm}
%package rocm
Requires: %{name}%{?_isa} = %{version}-%{release}
Summary: UCX ROCm GPU support

%description rocm
Provides Radeon Open Compute (ROCm) Runtime support for UCX.

%files rocm
%dir %{_libdir}/ucx
%{_libdir}/ucx/libuct_rocm.so.*
%{_libdir}/ucx/libucm_rocm.so.*

%if %{with gdrcopy}
%package rocmgdr
Requires: %{name}-rocm%{?_isa} = %{version}-%{release}
Summary: UCX GDRCopy support for ROCM

%description rocmgdr
Provide GDRCopy support for UCX ROCM. GDRCopy is a low-latency GPU
memory copy library, built on top of the NVIDIA GPUDirect RDMA
technology.

%files rocmgdr
%dir %{_libdir}/ucx
%{_libdir}/ucx/libuct_rocm_gdr.so.*
%endif
%endif

%if %{with ugni}
%package ugni
Requires: %{name}%{?_isa} = %{version}-%{release}
Summary: UCX Gemini/Aries transport support.

%description ugni
Provides Gemini/Aries transport for UCX.

%files ugni
%dir %{_libdir}/ucx
%{_libdir}/ucx/libuct_ugni.so.*
%endif

%if %{with xpmem}
%package xpmem
Requires: %{name}%{?_isa} = %{version}-%{release}
Summary: UCX XPMEM transport support.

%description xpmem
Provides XPMEM transport for UCX. XPMEM is a Linux kernel module that enables a
process to map the memory of another process into its virtual address space.

%files xpmem
%dir %{_libdir}/ucx
%{_libdir}/ucx/libuct_xpmem.so.*
%endif

%if %{with vfs}
%package vfs
Requires: %{name}%{?_isa} = %{version}-%{release}
Summary: UCX Virtual Filesystem support.

%description vfs
Provides a virtual filesystem over FUSE which allows real-time
monitoring of UCX library internals, protocol objects, transports
status, and more.

%files vfs
%dir %{_libdir}/ucx
%{_libdir}/ucx/libucs_fuse.so.*
%{_bindir}/ucx_vfs
%endif

%changelog
%autochangelog
