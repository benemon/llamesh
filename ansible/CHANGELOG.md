# Changelog

## Unreleased

- `llamesh_tls_cert` and `llamesh_tls_key`: with both set, the server serves the ingest port over TLS and its collectors connect with `-tls`.

## 0.1.0

- The `llamesh` role: installs a llamesh release and runs a server, a collector, or both, under launchd on macOS and systemd on Linux.
