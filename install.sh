#!/bin/sh
# Installs the latest chcli release on macOS or Linux into ~/.local/bin
# (or $CHCLI_INSTALL_DIR), after checking the archive's SHA-256 against the
# release's checksums.txt.
#
#   curl -fsSL https://raw.githubusercontent.com/nenych/chcli/main/install.sh | sh
set -eu

repo=nenych/chcli
dir=${CHCLI_INSTALL_DIR:-$HOME/.local/bin}

case $(uname -s) in
Darwin) os=darwin ;;
Linux) os=linux ;;
*)
	echo "This script supports macOS and Linux. On Windows, download a release archive from" >&2
	echo "https://github.com/$repo/releases or run: go install github.com/$repo/cmd/chcli@latest" >&2
	exit 1
	;;
esac
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*)
	echo "Unsupported CPU architecture: $(uname -m)" >&2
	exit 1
	;;
esac

asset=chcli_${os}_$arch.tar.gz
base=https://github.com/$repo/releases/latest/download
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $asset..."
curl -fsSL -o "$tmp/$asset" "$base/$asset"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

cd "$tmp"
if command -v sha256sum >/dev/null; then
	grep "  $asset\$" checksums.txt | sha256sum -c - >/dev/null
else
	grep "  $asset\$" checksums.txt | shasum -a 256 -c - >/dev/null
fi
tar -xzf "$asset"

mkdir -p "$dir"
install -m 755 chcli "$dir/chcli"
echo "Installed $("$dir/chcli" version) to $dir/chcli"

case ":$PATH:" in
*":$dir:"*) ;;
*) echo "Note: $dir is not in your PATH." ;;
esac

cat <<EOT

Shell completion scripts are in the archive's completions/ directory, or run:
  chcli completion zsh    # also bash, fish, powershell
EOT
