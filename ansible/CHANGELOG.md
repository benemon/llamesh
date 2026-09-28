# Changelog

## 0.2.1

- On macOS, the role creates `~/Library/LaunchAgents` when a new account has none.
- A collector whose server's host is not in the same run fails with a message saying so, before anything is installed.

## 0.2.0

- `llamesh_token_tls`, on by default: the server and its collectors run with `-token-tls`. Needs llamesh v0.2.0 or later; the role stops before installing an earlier release.
- `llamesh_tls_cert` and `llamesh_tls_key`: with both set, the server serves the ingest port over TLS with that certificate and its collectors connect with `-tls`, in place of token TLS.

## 0.1.0

- The `llamesh` role: installs a llamesh release and runs a server, a collector, or both, under launchd on macOS and systemd on Linux.
