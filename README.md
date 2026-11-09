# Ammonode Daemon

The node agent for [Ammonode Panel](https://github.com/Ammonode/panel). It runs
game servers in Docker containers and gives the panel a console, file manager,
SFTP and backups for each of them.

Eggs from [eggs.pterodactyl.io](https://eggs.pterodactyl.io/) run unchanged:
install scripts, startup variables, config file parsers, stop commands and the
Pterodactyl yolk images all behave the same way.

## Install

Requires Linux with Docker.

```bash
curl -sSL https://get.docker.com/ | CHANNEL=stable sh
systemctl enable --now docker

curl -L -o /usr/local/bin/ammonode-daemon \
  "https://github.com/Ammonode/daemon/releases/latest/download/ammonode-daemon_linux_$([[ "$(uname -m)" == "x86_64" ]] && echo amd64 || echo arm64)"
chmod +x /usr/local/bin/ammonode-daemon
```

In the panel open **Admin → Nodes → your node → Configuration**, copy the
`ammonode-daemon configure ...` command and run it on the node. Then:

```bash
curl -L -o /etc/systemd/system/ammonode-daemon.service \
  https://raw.githubusercontent.com/Ammonode/daemon/main/contrib/ammonode-daemon.service
systemctl enable --now ammonode-daemon
```

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
