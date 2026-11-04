#!/usr/bin/env bash
# Build CGO-free release binaries, stage the install files, and write SHA256SUMS.
# Usage: scripts/build-release.sh <version> <outdir>
# Modeled on the xstats script. deploy/bootstrap.sh downloads
# xpost-linux-<arch>, xpost.env.example and SHA256SUMS from the release.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ $# -ne 2 ] || [ -z "$1" ] || [ -z "$2" ]; then
	echo "usage: scripts/build-release.sh <version> <outdir>" >&2
	exit 2
fi

version=$1
outdir=$2
mkdir -p "$outdir"

for f in .env.example deploy/bootstrap.sh deploy/INSTALL.md; do
	if [ ! -f "$f" ]; then
		echo "build-release: missing $f" >&2
		exit 1
	fi
done
# Flat names so a release URL serves them next to the binaries.
cp -p .env.example "$outdir/xpost.env.example"
cp -p deploy/bootstrap.sh "$outdir/bootstrap.sh"
cp -p deploy/INSTALL.md "$outdir/INSTALL.md"

ldflags="-s -w -X main.version=${version}"
assets=()
for os in linux darwin; do
	for arch in amd64 arm64; do
		CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
			go build -trimpath -ldflags "$ldflags" -o "$outdir/xpost-$os-$arch" ./cmd/xpost
		assets+=("xpost-$os-$arch")
	done
done
assets+=(xpost.env.example bootstrap.sh INSTALL.md)

(
	cd "$outdir"
	sha256sum "${assets[@]}" > SHA256SUMS
)
