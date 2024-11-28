#!/bin/bash

__DIR__="$( cd "$( dirname "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"

set -e

pkgdir=$(readlink -f ${__DIR__}/../packages)
scriptdir=${__DIR__}
repodir=$(readlink -f ${__DIR__}/..)

repobasename=slurm
reponame=${repobasename}

os=rocky+epel
releasever=9
arch=$(rpm --eval "%{_arch}") # x86_64
mockroot=${os}-${releasever}-${arch}
releasedir=el${releasever}
builddir=${repodir}/build/${releasedir}
resultdir=${repodir}/result/${releasedir}

mocketc=/etc/mock
mocktemplates=${mocketc}/templates
mock="mock -r ${mockroot} --macro-file=${scriptdir}/macros --enable-plugin=rpmautospec"
rpmspec="rpmspec --load=${scriptdir}/macros"

mkdir -p ${builddir}
mkdir -p ${resultdir}

function status() {
  echo
  tree -h ${builddir} ${resultdir}
  echo
  du -hs ${builddir} ${resultdir}
}
