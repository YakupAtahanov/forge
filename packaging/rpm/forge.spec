Name:           forge
Version:        1.0.0
Release:        1%{?dist}
Summary:        Git-based version control CLI for 3D models and game assets

License:        MIT
URL:            https://github.com/forgehubproject/forge
Source0:        https://github.com/forgehubproject/forge/archive/refs/tags/v%{version}.tar.gz#/forge-%{version}.tar.gz

BuildRequires:  golang >= 1.21
BuildRequires:  git

%description
Forge layers semantic diff and merge on top of git, enabling humans and
AI tools to clearly see changes in 3D models (glTF), game assets, and
other binary formats that git treats as opaque blobs.

Supports a plugin system (FHR) for adding handlers for any file format,
and includes an MCP server (forge mcp) for AI tool integration.

%prep
%autosetup -n forge-%{version}

%build
export CGO_ENABLED=0
go build \
    -trimpath \
    -ldflags "-s -w -X main.version=%{version}" \
    -o forge \
    ./cmd/forge

%install
install -Dm755 forge %{buildroot}%{_bindir}/forge

%check
go test ./...

%files
%license LICENSE
%{_bindir}/forge

%changelog
* Mon Sep 08 2026 Toufic Majdalani <toufic@touficmajdalani.com> - 1.0.0-1
- Initial release
- Git-based CLI with semantic diff and merge for 3D models, game assets,
  and binary formats via the FHR plugin system
- Includes MCP server (forge mcp) for AI tool integration
