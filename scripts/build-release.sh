#!/bin/sh
# Builds the release archives for every platform into dist/ (or the directory
# given as the second argument), with a checksums.txt next to them. chcli needs
# no cgo, so every target is cross-compiled from wherever this runs.
#
#   scripts/build-release.sh v1.2.3 [dist]
set -eu

cd "$(dirname "$0")/.."
tag=${1:?usage: build-release.sh <tag> [dist]}
version=${tag#v}
dist=${2:-dist}
commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
mkdir -p "$dist"

# Shell completions are the same on every platform; generate them once.
completions=$dist/completions
mkdir -p "$completions"
for shell in bash zsh fish powershell; do
	ext=$shell
	[ "$shell" = powershell ] && ext=ps1
	go run ./cmd/chcli completion "$shell" > "$completions/chcli.$ext"
done

build() {
	goos=$1
	goarch=$2
	name=chcli_${goos}_$goarch
	work=$dist/$name
	rm -rf "$work"
	mkdir -p "$work"
	bin=chcli
	[ "$goos" = windows ] && bin=chcli.exe

	GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -X main.version=$version -X main.commit=$commit -X main.date=$date" \
		-o "$work/$bin" ./cmd/chcli
	cp LICENSE THIRD_PARTY_NOTICES.md README.md "$work/"
	cp -R "$completions" "$work/completions"

	if [ "$goos" = windows ]; then
		(cd "$work" && zip -qr "../$name.zip" .)
	else
		tar -czf "$dist/$name.tar.gz" -C "$work" .
	fi
	rm -rf "$work"
	echo "built $name"
}

build darwin arm64
build darwin amd64
build linux amd64
build linux arm64
build windows amd64
build windows arm64

rm -rf "$completions"
(cd "$dist" && shasum -a 256 ./*.tar.gz ./*.zip | sed 's|\./||' > checksums.txt)
echo "checksums:"
cat "$dist/checksums.txt"
