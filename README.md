# Ammonode Daemon

The node agent for [Ammonode Panel](https://github.com/Ammonode/panel). It runs
game servers in Docker containers and gives the panel a console, file manager,
SFTP and backups for each of them.

Eggs from [eggs.pterodactyl.io](https://eggs.pterodactyl.io/) run unchanged:
install scripts, startup variables, config file parsers, stop commands and the
Pterodactyl yolk images all behave the same way.

## Install

Requires Linux (amd64 or arm64). The script installs Docker when missing,
the daemon binary and a systemd service:

```bash
curl -sSL https://raw.githubusercontent.com/Ammonode/daemon/main/install.sh | sudo bash
```

In the panel open **Admin → Nodes → your node**, copy the configure command,
run it on the node and start the daemon:

```bash
systemctl enable --now ammonode-daemon
```

Full guide: https://ammonode.github.io/docs/guide/install-daemon

## Configuration

`/etc/ammonode/config.yml`. Useful options:

| Key | Default | |
|---|---|---|
| `api.port` | `8080` | HTTP/WebSocket API |
| `api.ssl.enabled` | `false` | Serve TLS with `api.ssl.cert` / `api.ssl.key` |
| `system.data` | `/var/lib/ammonode/volumes` | Server files |
| `system.sftp.bind_port` | `2022` | SFTP |
| `system.crash_detection.enabled` | `true` | Restart servers that exit unexpectedly |
| `docker.network.subnet` | `172.18.0.0/16` | Bridge network for servers |

## Development

```bash
make build      # build/ammonode-daemon
make test
```

## License

MIT
