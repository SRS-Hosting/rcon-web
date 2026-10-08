# rcon-web

[![Release](https://github.com/SRS-Hosting/rcon-web/actions/workflows/release.yaml/badge.svg)](https://github.com/SRS-Hosting/rcon-web/actions/workflows/release.yaml) [![License](https://badgen.net/github/license/SRS-Hosting/rcon-web)](https://github.com/SRS-Hosting/rcon-web/blob/main/LICENSE) [![Version](https://img.shields.io/github/release/SRS-Hosting/rcon-web.svg)](https://github.com/SRS-Hosting/rcon-web/releases/) [![Coverage](.github/badges/coverage.svg)](https://github.com/SRS-Hosting/rcon-web/actions/workflows/test.yaml)

Simple RCON web terminal.

## Configuration

<!-- configulator:begin -->

| Key                   | Type    | Default     | Environment           | Flag                    | Description                                                                                  |
|-----------------------|---------|-------------|-----------------------|-------------------------|----------------------------------------------------------------------------------------------|
| `logLevel`            | string  | `info`      | `LOGLEVEL`            | `--logLevel`            | log verbosity: debug, info, warn, or error                                                   |
| `http.bind`           | string  |             | `HTTP_BIND`           | `--http.bind`           | address to listen on; empty listens on all interfaces over both IPv4 and IPv6                |
| `http.port`           | integer | `8080`      | `HTTP_PORT`           | `--http.port`           | port the web interface listens on                                                            |
| `rcon.host`           | string  | `127.0.0.1` | `RCON_HOST`           | `--rcon.host`           | hostname or IP of the Source RCON server                                                     |
| `rcon.port`           | integer | `27015`     | `RCON_PORT`           | `--rcon.port`           | TCP port of the Source RCON server                                                           |
| `rcon.password`       | string  |             | `RCON_PASSWORD`       | `--rcon.password`       | RCON password (required) (secret)                                                            |
| `rcon.timeoutSeconds` | integer | `5`         | `RCON_TIMEOUTSECONDS` | `--rcon.timeoutSeconds` | deadline in seconds covering a whole RCON exchange: connect, authenticate, command, response |
| `rcon.maxConcurrent`  | integer | `4`         | `RCON_MAXCONCURRENT`  | `--rcon.maxConcurrent`  | maximum RCON commands in flight at once; further requests get HTTP 503                       |

<!-- configulator:end -->
