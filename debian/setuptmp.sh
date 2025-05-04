#!/bin/sh

# call from debian/
set -x

ver=6.4.0

p=rocm-smi
n=rocm_smi_lib
b=$HOME/b/$p
if [ -d $b ]; then
    rm -rf $b
fi

mkdir -p $HOME/b
rsync -ar ../../$p $HOME/b

cd $HOME/b/$p
    
tar xf ${n}-${ver}.tar.gz
mv ${n}-rocm-${ver} ${p}-${ver}

tar cf ${p}_${ver}.orig.tar ${p}-${ver}
gzip --fast ${p}_${ver}.orig.tar

cd ${p}-${ver}

mv ../debian .

