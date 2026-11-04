# Install xpost on a server

You need an Ubuntu (or Debian) server with systemd, a DNS name pointing at it, and ports 80 and 443 open.

1. Copy `deploy/bootstrap.sh` to the server, and also `.env.example` if you are installing from a checkout. Then run it as root:

   ```
   sudo bash bootstrap.sh --domain post.example.com --email you@example.com --owner-email you@example.com
   ```

   Without `--binary FILE`, it downloads the release binary for your CPU (`xpost-linux-amd64` or `arm64`) and checks it against `SHA256SUMS`. `XPOST_RELEASE_URL` or `--release-url` changes where it downloads from.

2. Open the one-time setup link it prints. Set a password and add a passkey.

3. In Settings > X accounts, add the X handle you post as.

4. To connect X, set `XPOST_X_CLIENT_ID` and `XPOST_X_CLIENT_SECRET_FILE` in `/etc/xpost/env`, then run `systemctl restart xpost`. The callback URL is shown in Settings > X accounts.

Posting stays off until you turn on the scheduler and set the cost rates on purpose (see the posting section of `.env.example`). Blogs stay off unless you set `XPOST_BLOGS_ENABLED=1`.

Re-running bootstrap is safe:
- It keeps the master key, the database and every value in `/etc/xpost/env`. It only adds keys that are missing.
- It rewrites the systemd unit and `/etc/caddy/xpost.caddy` only when they differ.
- It never edits other sites in `/etc/caddy/Caddyfile`. It adds a single `import` line.

For a test machine without public DNS, use `--internal-tls`. Caddy then uses its own CA instead of Let's Encrypt.
