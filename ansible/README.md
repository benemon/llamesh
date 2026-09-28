# benemon.llamesh

An Ansible collection with one role, `llamesh`, that installs llamesh from the project's GitHub releases and runs it as a service: a server, a collector, or both on one host.

The control node downloads each release once, checks every archive against the release's `SHA256SUMS`, and copies the binary for each host's OS and architecture. Hosts need no route to GitHub. On macOS the role installs user LaunchAgents; on Linux, systemd units.

## Requirements

- Ansible 2.16 or later on the control node.
- macOS, or Linux with systemd, on the hosts; amd64 or arm64.
- On macOS, the connecting user logged in to the desktop: LaunchAgents load into that user's session.
- `cosign` on the control node when `llamesh_verify_signature` is true.

## Installation

```sh
ansible-galaxy collection install git+https://github.com/benemon/llamesh.git#/ansible
```

## Usage

`examples/inventory.yml` and `examples/site.yml` describe one macOS host running the server and a collector, and one Linux host running a collector.

1. List the hosts. Set `llamesh_server: true` on the server's host.
2. Set `llamesh_collector` to the server's ingest address and `llamesh_server_host` to the server's inventory name on every host that runs a collector.
3. Pin `llamesh_version` to a release tag. The default, `latest`, moves hosts to each new release on the next run.
4. Run the playbook:

   ```sh
   ansible-playbook -i inventory.yml site.yml
   ```

The server's host generates the ingest token once and keeps it at `/etc/llamesh/token` on Linux or `~/.config/llamesh/token` on macOS. Collectors read it from there, so the server's host must be in every play that configures a collector.

The server's host chooses how collectors connect, and the collectors that report to it follow:

- Token TLS, the default: the server and its collectors run with `-token-tls`, which authenticates the TLS connection with the token and needs no certificates. It needs `llamesh_version` v0.2.0 or later; the role stops before installing an earlier release.
- Certificate TLS: set both `llamesh_tls_cert` and `llamesh_tls_key` on the server's host, and they take precedence over token TLS. Collectors check the certificate against their host's trusted roots and the address in `llamesh_collector`, so it must chain to a CA each collector's host trusts and name that address; an IP address needs an IP subject alternative name. On Linux the server reads them through `LoadCredential=`, which needs systemd 248 or later.
- Plaintext: set `llamesh_token_tls: false` and leave the certificate and key empty.

[The main README](../README.md#connections) compares the three.

A collector on macOS that reports to another host needs the Local Network permission for the binary, granted once in System Settings. See [the main README](../README.md) for the permissions and the llama-server flags llamesh reads.

## Role variables

| Variable | Default | Meaning |
|---|---|---|
| `llamesh_version` | `latest` | A release tag, or `latest`. |
| `llamesh_repo` | `benemon/llamesh` | The GitHub repository releases come from. |
| `llamesh_cache_dir` | `~/.cache/llamesh` | Where the control node keeps downloaded releases. |
| `llamesh_verify_signature` | `false` | Verify the keyless cosign signature over `SHA256SUMS` before trusting it. |
| `llamesh_server` | `false` | Run the server on this host. |
| `llamesh_listen` | `127.0.0.1:8899` | The server's address for the page. |
| `llamesh_ingest` | `:8900` | The server's address for collectors. |
| `llamesh_collector` | `""` | Run a collector reporting to this `host:port`. |
| `llamesh_server_host` | `""` | The server's inventory name, whose token collectors read. |
| `llamesh_poll` | `1s` | How often a collector reads its llama-servers. |
| `llamesh_token_tls` | `true` | TLS on the ingest port authenticated by the token; needs v0.2.0 or later. |
| `llamesh_tls_cert` | `""` | The server's certificate as PEM. |
| `llamesh_tls_key` | `""` | The server's private key as PEM. |
| `llamesh_collector_user` | `root` | Linux only: the collector's user. It reads the llama-servers' logs and `/proc` entries, so it is their user or root. |

## Services

| | macOS | Linux |
|---|---|---|
| Server | `io.github.benemon.llamesh.server` | `llamesh-server.service` |
| Collector | `io.github.benemon.llamesh.collector` | `llamesh-collector.service` |
| Logs | `~/Library/Logs/llamesh-*.log` | `journalctl -u llamesh-*` |
| Binary | `~/.local/bin/llamesh` | `/usr/local/bin/llamesh` |

A changed binary, unit, token, certificate or key restarts the services that use it.
