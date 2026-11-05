#!/usr/bin/env bash
# Install xpost on an Ubuntu (or Debian) server with systemd and Caddy.
# Run as root on the server. Safe to re-run: it changes only what is
# missing or out of date and says what it kept.
#
#   sudo bash deploy/bootstrap.sh --domain post.example.com --email you@example.com
#
# What it does:
#   - creates the xpost system user and /opt/xpost, /var/lib/xpost, /etc/xpost
#   - installs the xpost binary (--binary FILE, or a release download checked
#     against SHA256SUMS), keeping the previous one as xpost.prev
#   - generates the master key once (/etc/xpost/key)
#   - creates /etc/xpost/env from .env.example (mode 600, root) and on later
#     runs only adds keys that are missing; it never changes a value
#   - writes the systemd unit and /etc/caddy/xpost.caddy, and imports that
#     file from /etc/caddy/Caddyfile without touching other sites
#   - installs Caddy from its official apt repo when it is missing
#   - starts xpost and Caddy and checks /healthz
#   - with --owner-email, creates the owner once and prints the setup link
#
# Posting stays off: XPOST_SCHEDULER is not set. Blogs stay off.
# It never touches sshd, the firewall or other services.

case $- in *x*) printf 'refusing to run with xtrace enabled\n' >&2; exit 1 ;; esac
set +x
set -euo pipefail

usage() {
	cat <<'EOF'
usage: bootstrap.sh --domain HOST [--email ADDR] [--binary FILE | --release-url URL]
                    [--env-example FILE] [--port N] [--tz ZONE] [--internal-tls]
                    [--owner-email ADDR] [--owner-name NAME]

  --domain HOST        public host name xpost serves, e.g. post.example.com
                       (or XPOST_DOMAIN; prompted on a terminal)
  --email ADDR         contact email for Let's Encrypt (optional)
  --binary FILE        install this xpost binary instead of downloading one
  --release-url URL    download base with xpost-linux-<arch> and SHA256SUMS
                       (default: XPOST_RELEASE_URL or the latest GitHub release)
  --env-example FILE   template for /etc/xpost/env (default: .env.example next
                       to this script's directory, else xpost.env.example from
                       the release, checked against SHA256SUMS)
  --port N             loopback port xpost listens on (default 8091)
  --tz ZONE            default display zone (IANA), written once as XPOST_TZ
  --internal-tls       Caddy's own CA instead of Let's Encrypt (testing, LANs)
  --owner-email ADDR   create the owner if there is none and print the
                       one-time setup link
  --owner-name NAME    the owner's display name (default: Owner)
EOF
}

say() { printf 'bootstrap: %s\n' "$*"; }
die() { printf 'bootstrap: ERROR: %s\n' "$*" >&2; exit 1; }

valid_host() {
	[ "${#1}" -le 253 ] && [[ "$1" =~ ^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$ ]]
}
valid_email() { [[ "$1" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]]; }
prompt() {
	local v
	[ -r /dev/tty ] || return 1
	printf '%s' "$1" >/dev/tty
	IFS= read -r v </dev/tty || return 1
	printf '%s' "${v//$'\r'/}"
}

domain=${XPOST_DOMAIN:-}
email=${XPOST_ACME_EMAIL:-}
binary=
release_url=${XPOST_RELEASE_URL:-https://github.com/scott/xpost/releases/latest/download}
env_example=
port=8091
tz=
internal_tls=0
owner_email=
owner_name=Owner
while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help) usage; exit 0 ;;
	--domain | --email | --binary | --release-url | --env-example | --port | --tz | --owner-email | --owner-name)
		[ $# -ge 2 ] || { usage >&2; exit 2; }
		case "$1" in
		--domain) domain=$2 ;; --email) email=$2 ;; --binary) binary=$2 ;;
		--release-url) release_url=$2 ;; --env-example) env_example=$2 ;; --port) port=$2 ;;
		--tz) tz=$2 ;; --owner-email) owner_email=$2 ;; --owner-name) owner_name=$2 ;;
		esac
		shift 2 ;;
	--internal-tls) internal_tls=1; shift ;;
	*) printf 'unknown argument: %s\n' "$1" >&2; usage >&2; exit 2 ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run as root (sudo bash deploy/bootstrap.sh ...)"
if [ -z "$domain" ]; then
	domain=$(prompt "Domain xpost serves (e.g. post.example.com): ") || die "set --domain or XPOST_DOMAIN"
fi
domain=${domain,,}
valid_host "$domain" || die "invalid domain: $domain"
if [ -z "$email" ] && [ "$internal_tls" -eq 0 ] && [ -r /dev/tty ]; then
	email=$(prompt "Email for Let's Encrypt notices (Enter to skip): ") || email=
fi
[ -z "$email" ] || valid_email "$email" || die "invalid email"
[ -z "$owner_email" ] || valid_email "$owner_email" || die "invalid owner email"
[[ "$port" =~ ^[0-9]{2,5}$ ]] && [ "$port" -ge 1024 ] && [ "$port" -le 65535 ] || die "invalid port: $port"
[[ "$owner_name" =~ ^[[:print:]]{1,60}$ ]] || die "invalid owner name"
case "$tz" in *[[:space:]=]*) die "invalid zone: $tz" ;; esac
command -v systemctl >/dev/null || die "systemd (systemctl) is required"
grep -qE '^ID(_LIKE)?=.*(ubuntu|debian)' /etc/os-release 2>/dev/null || say "warning: not Ubuntu or Debian; continuing"

# When this script is piped into bash, BASH_SOURCE is empty and dirname
# would land on the current directory. Only trust a real on-disk path.
script_dir=
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
	script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fi
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

ensure_tools() {
	local miss=()
	for c in curl sha256sum runuser; do command -v "$c" >/dev/null || miss+=("$c"); done
	[ ${#miss[@]} -eq 0 ] && return 0
	say "installing curl, coreutils, util-linux"
	DEBIAN_FRONTEND=noninteractive apt-get update -qq
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates curl coreutils util-linux >/dev/null
}
download() { curl -fsSL --retry 2 -o "$2" "$1" && [ -s "$2" ] || die "download failed: $1"; }
checksum() { # file name: the release SHA256SUMS must list name exactly once and match
	local want
	want=$(tr -d '\r' <"$tmp/SHA256SUMS" | awk -v n="$2" '$2 == n || $2 == "*" n { print tolower($1) }')
	[ -n "$want" ] && [ "$(printf '%s\n' "$want" | wc -l)" = 1 ] || die "SHA256SUMS has no single entry for $2"
	[ "$want" = "$(sha256sum "$1" | awk '{print tolower($1)}')" ] || die "checksum mismatch for $2"
}
changed() { ! [ -f "$2" ] || ! cmp -s "$1" "$2"; }

ensure_tools
arch=$(uname -m)
case "$arch" in x86_64 | amd64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; *) die "unsupported architecture: $arch" ;; esac

# --- the binary and the env template, staged and checked first ---
if [ -n "$binary" ]; then
	[ -f "$binary" ] || die "no such binary: $binary"
	cp "$binary" "$tmp/xpost"
else
	release_url=${release_url%/}
	download "$release_url/SHA256SUMS" "$tmp/SHA256SUMS"
	download "$release_url/xpost-linux-$arch" "$tmp/xpost"
	checksum "$tmp/xpost" "xpost-linux-$arch"
	say "checksum ok for xpost-linux-$arch"
fi
chmod 0755 "$tmp/xpost"
version=$("$tmp/xpost" version 2>/dev/null | awk '{print $NF}') && [ -n "$version" ] || die "the xpost binary does not run on this server"
if [ -n "$tz" ]; then # checked with this binary before anything is installed
	tzerr=$(XPOST_KEY=$("$tmp/xpost" keygen) "$tmp/xpost" create-owner -db "$tmp/tzcheck.db" -email check@example.invalid -name check -tz "$tz" 2>&1 >/dev/null) || die "invalid --tz $tz: $tzerr"
	rm -f "$tmp"/tzcheck.db*
fi
if [ -z "$env_example" ] && [ -n "$script_dir" ] && [ -f "$script_dir/../.env.example" ]; then env_example=$script_dir/../.env.example; fi
if [ -z "$env_example" ]; then
	[ -f "$tmp/SHA256SUMS" ] || download "${release_url%/}/SHA256SUMS" "$tmp/SHA256SUMS"
	download "${release_url%/}/xpost.env.example" "$tmp/env.example"
	checksum "$tmp/env.example" xpost.env.example
	env_example=$tmp/env.example
fi
[ -f "$env_example" ] || die "no env template: $env_example"
say "installing xpost $version for $domain"

# --- user and layout ---
if id xpost >/dev/null 2>&1; then say "user xpost exists"; else
	useradd --system --home-dir /var/lib/xpost --no-create-home --shell /usr/sbin/nologin --user-group xpost
	say "user xpost created"
fi
install -d -m 0755 -o root -g root /opt/xpost /opt/xpost/bin
install -d -m 0750 -o xpost -g xpost /var/lib/xpost
install -d -m 0750 -o root -g xpost /etc/xpost

# --- binary ---
bin_changed=0
if changed "$tmp/xpost" /opt/xpost/bin/xpost; then
	[ -f /opt/xpost/bin/xpost ] && cp -p /opt/xpost/bin/xpost /opt/xpost/bin/xpost.prev
	install -m 0755 "$tmp/xpost" /opt/xpost/bin/xpost.new
	mv -f /opt/xpost/bin/xpost.new /opt/xpost/bin/xpost
	bin_changed=1
	say "binary installed: /opt/xpost/bin/xpost ($version)"
else
	say "binary unchanged ($version)"
fi

# --- master key: generated once; it never leaves the server ---
if [ -s /etc/xpost/key ]; then say "key exists; kept (a new one would orphan stored X tokens)"; else
	(umask 077; /opt/xpost/bin/xpost keygen >/etc/xpost/key.new)
	[ -s /etc/xpost/key.new ] || die "keygen produced nothing"
	mv -n /etc/xpost/key.new /etc/xpost/key
	rm -f /etc/xpost/key.new
	say "key generated"
fi
chown root:xpost /etc/xpost/key
chmod 0640 /etc/xpost/key

# --- env: from the template; existing values are never changed ---
env_changed=0
declare -A want=([XPOST_BASE_URL]="https://$domain" [XPOST_DB]=/var/lib/xpost/xpost.db [XPOST_KEY_FILE]=/etc/xpost/key)
[ -z "$tz" ] || want[XPOST_TZ]=$tz
env_file=/etc/xpost/env
cp -p "$env_file" "$tmp/env.old" 2>/dev/null || : >"$tmp/env.old"
if [ -s "$env_file" ]; then cp "$env_file" "$tmp/env"; else cp "$env_example" "$tmp/env"; fi
active() { grep -qE "^$1=" "$tmp/env"; }
for k in "${!want[@]}"; do
	if grep -qE "^$k=$" "$tmp/env"; then # an empty line from the template
		sed -i "s|^$k=\$|$k=${want[$k]}|" "$tmp/env"
	elif ! active "$k"; then
		printf '%s=%s\n' "$k" "${want[$k]}" >>"$tmp/env"
	fi
done
# Keys the template knows that this env lacks entirely: add them as the
# template has them (commented out = off), so a newer xpost's options show.
added=0
while IFS= read -r line; do
	k=$(printf '%s' "$line" | sed -nE 's/^#? ?(XPOST_[A-Z0-9_]+)=.*/\1/p')
	[ -n "$k" ] || continue
	grep -qE "^#? ?$k=" "$tmp/env" && continue
	[ $added = 1 ] || printf '\n# Added by bootstrap from a newer .env.example:\n' >>"$tmp/env"
	printf '%s\n' "$line" >>"$tmp/env"; added=1
done <"$env_example"
if cmp -s "$tmp/env" "$tmp/env.old"; then say "env unchanged ($env_file)"; else
	[ -s "$tmp/env.old" ] && cp -p "$tmp/env.old" "$env_file.bak-$(date -u +%Y%m%dT%H%M%SZ)"
	install -m 0600 -o root -g root "$tmp/env" "$env_file.new"
	mv -f "$env_file.new" "$env_file"
	env_changed=1
	say "env written ($env_file, mode 600; existing values kept)"
fi
chown root:root "$env_file"
chmod 0600 "$env_file"
envval() { sed -n "s/^$1=//p" "$env_file" | tail -n 1; }

# --- systemd unit ---
cat >"$tmp/xpost.service" <<EOF
# Written by xpost deploy/bootstrap.sh. Re-running bootstrap rewrites it.
[Unit]
Description=xpost (X post review UI and agent draft API)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=xpost
Group=xpost
WorkingDirectory=/var/lib/xpost
EnvironmentFile=/etc/xpost/env
ExecStart=/opt/xpost/bin/xpost serve -addr 127.0.0.1:$port -trust-proxy
Restart=on-failure
RestartSec=3
UMask=0077
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/xpost
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
CapabilityBoundingSet=
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
EOF
unit_changed=0
if changed "$tmp/xpost.service" /etc/systemd/system/xpost.service; then
	install -m 0644 "$tmp/xpost.service" /etc/systemd/system/xpost.service
	systemctl daemon-reload
	unit_changed=1
	say "systemd unit written"
else
	say "systemd unit unchanged"
fi

# --- Caddy ---
if command -v caddy >/dev/null; then say "caddy present ($(caddy version 2>/dev/null | awk '{print $1}'))"; else
	say "installing Caddy from its official apt repo"
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -qq
	apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https curl gpg >/dev/null
	curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
	curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt >/etc/apt/sources.list.d/caddy-stable.list
	apt-get update -qq
	apt-get install -y -qq caddy >/dev/null
fi
tls_line=
if [ "$internal_tls" -eq 1 ]; then tls_line=$'\ttls internal'; elif [ -n "$email" ]; then tls_line=$'\t'"tls $email"; fi
{
	printf '# xpost site. Written by xpost deploy/bootstrap.sh; re-running it rewrites this file.\n'
	printf '%s {\n' "$domain"
	[ -z "$tls_line" ] || printf '%s\n' "$tls_line"
	printf '\tencode gzip\n\theader {\n\t\tStrict-Transport-Security "max-age=31536000"\n\t\t-Server\n\t}\n'
	printf '\t# The app caps a form upload at 81 MB; refuse more here.\n\trequest_body {\n\t\tmax_size 96MB\n\t}\n'
	printf '\treverse_proxy 127.0.0.1:%s\n}\n' "$port"
} >"$tmp/xpost.caddy"
install -d -m 0755 /etc/caddy
[ -f /etc/caddy/Caddyfile ] || : >/etc/caddy/Caddyfile
stamp=$(date -u +%Y%m%dT%H%M%SZ)
cp -p /etc/caddy/Caddyfile "$tmp/Caddyfile.old"
had_site=0; [ -f /etc/caddy/xpost.caddy ] && { cp -p /etc/caddy/xpost.caddy "$tmp/xpost.caddy.old"; had_site=1; }
caddy_changed=0
if changed "$tmp/xpost.caddy" /etc/caddy/xpost.caddy; then
	install -m 0644 "$tmp/xpost.caddy" /etc/caddy/xpost.caddy; caddy_changed=1
fi
if grep -qE '^[[:space:]]*import[[:space:]]+/etc/caddy/(xpost\.caddy|\*\.caddy)[[:space:]]*$' /etc/caddy/Caddyfile; then :; else
	[ -z "$(tail -c 1 /etc/caddy/Caddyfile)" ] || echo >>/etc/caddy/Caddyfile
	printf '\n# xpost: added by xpost deploy/bootstrap.sh. Keep this import; it serves %s.\nimport /etc/caddy/xpost.caddy\n' "$domain" >>/etc/caddy/Caddyfile
	caddy_changed=1
fi
if [ "$caddy_changed" = 1 ]; then
	if ! caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >"$tmp/validate.log" 2>&1; then
		cp -p "$tmp/Caddyfile.old" /etc/caddy/Caddyfile
		if [ $had_site = 1 ]; then cp -p "$tmp/xpost.caddy.old" /etc/caddy/xpost.caddy; else rm -f /etc/caddy/xpost.caddy; fi
		tail -n 5 "$tmp/validate.log" >&2
		die "caddy validate failed; Caddyfile and xpost.caddy restored"
	fi
	cp -p "$tmp/Caddyfile.old" "/etc/caddy/Caddyfile.bak-xpost-$stamp"
	say "caddy config written and validated (backup /etc/caddy/Caddyfile.bak-xpost-$stamp)"
else
	say "caddy config unchanged"
fi

# --- start ---
systemctl enable -q xpost caddy
if [ $bin_changed = 1 ] || [ $unit_changed = 1 ] || [ $env_changed = 1 ] || ! systemctl is-active -q xpost; then
	systemctl restart xpost; say "xpost (re)started"
else
	say "xpost already running; left as is"
fi
if ! systemctl is-active -q caddy; then systemctl restart caddy; say "caddy started"
elif [ $caddy_changed = 1 ]; then systemctl reload caddy; say "caddy reloaded"
else say "caddy running; left as is"; fi

ok=0
for _ in $(seq 1 30); do
	if [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/healthz")" = 200 ]; then ok=1; break; fi
	sleep 1
done
[ $ok = 1 ] || die "xpost did not answer /healthz on 127.0.0.1:$port (journalctl -u xpost)"
say "healthz ok on 127.0.0.1:$port"

# --- owner (optional, once) ---
xp() { runuser -u xpost -- env XPOST_DB="$(envval XPOST_DB)" XPOST_KEY_FILE="$(envval XPOST_KEY_FILE)" XPOST_BASE_URL="$(envval XPOST_BASE_URL)" /opt/xpost/bin/xpost "$@"; }
if [ -n "$owner_email" ]; then
	if out=$(xp create-owner -email "$owner_email" -name "$owner_name" 2>&1); then
		say "owner created. Open this one-time link to set a password and passkey:"
		printf '%s\n' "$out" | grep -E '^https?://' || printf '%s\n' "$out"
	elif printf '%s' "$out" | grep -qi 'exist'; then
		say "owner exists; kept (new link: sudo -u xpost ... xpost setup-link)"
	else
		printf '%s\n' "$out" >&2; die "create-owner failed"
	fi
fi

say "done: https://$domain (posting stays off; blogs off; X accounts: add them in Settings)"
say "next: set XPOST_X_CLIENT_ID and the client secret file in $env_file, then: systemctl restart xpost"
