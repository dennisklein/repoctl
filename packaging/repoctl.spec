# The tool RPM of repoctl (docs/design/tool.html#pages). The release job
# builds it in a fedora:N container for each Fedora release of the RPM repo.
# The Go binary comes prebuilt in Source0 (packaging/build.sh): the golang
# of a Fedora release can be older than the Go of go.mod.
%global debug_package %{nil}

Name:           repoctl
Version:        %{repoctl_version}
Release:        1%{?dist}
Summary:        Build, sign and publish RPM package sets
License:        Apache-2.0
URL:            https://github.com/dennisklein/repoctl
Source0:        repoctl-%{version}.tar.gz
ExclusiveArch:  x86_64

# The system tools of an admin host: one dnf install gives a ready host.
Requires:       mock
Requires:       mock-rpmautospec
Requires:       podman
Requires:       rpm-sign
Requires:       createrepo_c
Requires:       git
Requires:       gnupg2

%description
repoctl builds a package set of RPMs with mock, signs the RPMs and publishes
signed snapshots of each target to an OCI registry. It also checks the set
for advisories, runs a local web UI and an MCP server for agents.

%prep
%setup -q

%build
# packaging/build.sh built the static binary with the Go of go.mod.

%install
install -D -m 0755 repoctl %{buildroot}%{_bindir}/repoctl
install -D -m 0644 completions/repoctl.bash %{buildroot}%{_datadir}/bash-completion/completions/repoctl
install -D -m 0644 completions/_repoctl %{buildroot}%{_datadir}/zsh/site-functions/_repoctl
install -D -m 0644 completions/repoctl.fish %{buildroot}%{_datadir}/fish/vendor_completions.d/repoctl.fish

%files
%license LICENSE
%doc README.md
%{_bindir}/repoctl
%{_datadir}/bash-completion/completions/repoctl
%{_datadir}/zsh/site-functions/_repoctl
%{_datadir}/fish/vendor_completions.d/repoctl.fish
