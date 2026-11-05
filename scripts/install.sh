#!/usr/bin/env bash
# One-line installer for xpost. Downloads bootstrap.sh from the latest
# GitHub release, checks it against SHA256SUMS, then runs it.
#
#   curl -fsSL https://raw.githubusercontent.com/scott/xpost/main/scripts/install.sh \
#     | sudo bash -s -- --domain post.example.com --email you@example.com --owner-email you@example.com
#
# Extra flags are passed through to bootstrap.sh (see --help).
# Override the download base with XPOST_RELEASE_URL.

case $- in *x*) printf 'refusing to run with xtrace enabled\n' >&2; exit 1 ;; esac
set +x
set -euo pipefail

die() { printf 'install: ERROR: %s\n' "$*" >&2; exit 1; }
say() { printf 'install: %s\n' "$*"; }

[ "$(id -u)" -eq 0 ] || die "run as root (curl … | sudo bash -s -- …)"
command -v curl >/dev/null || die "curl is required"
command -v sha256sum >/dev/null || die "sha256sum is required"

release_url=${XPOST_RELEASE_URL:-https://github.com/scott/xpost/releases/latest/download}
release_url=${release_url%/}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "downloading bootstrap from $release_url"
curl -fsSL --retry 2 -o "$tmp/SHA256SUMS" "$release_url/SHA256SUMS" || die "download failed: SHA256SUMS"
curl -fsSL --retry 2 -o "$tmp/bootstrap.sh" "$release_url/bootstrap.sh" || die "download failed: bootstrap.sh"
[ -s "$tmp/SHA256SUMS" ] && [ -s "$tmp/bootstrap.sh" ] || die "empty download"

want=$(tr -d '\r' <"$tmp/SHA256SUMS" | awk '$2 == "bootstrap.sh" || $2 == "*bootstrap.sh" { print tolower($1) }')
[ -n "$want" ] && [ "$(printf '%s\n' "$want" | wc -l)" = 1 ] || die "SHA256SUMS has no single entry for bootstrap.sh"
got=$(sha256sum "$tmp/bootstrap.sh" | awk '{print tolower($1)}')
[ "$want" = "$got" ] || die "checksum mismatch for bootstrap.sh"
say "checksum ok for bootstrap.sh"

chmod 0755 "$tmp/bootstrap.sh"
exec bash "$tmp/bootstrap.sh" "$@"
