# Mailglo CLI

`mailglo` runs any Mailglo API operation from a terminal, a script or an AI agent. It
uses the same permissions as the Mailglo web app and prints JSON.

You do not need a new CLI release when the API gains an operation. The CLI reads its
commands from the server's API description (the OpenAPI document at
`https://mailglo.com/api/openapi.json`), so it always matches the server you use.

## Install

**Homebrew**:

```sh
brew install --cask mailglo/tap/mailglo
```

**Release download**: get the archive for your system from
[Releases](https://github.com/mailglo/cli/releases), check it against
`checksums.txt`, and put `mailglo` on your `PATH`:

```sh
sha256sum --ignore-missing -c checksums.txt   # macOS: shasum -a 256 --ignore-missing -c checksums.txt
tar -xzf mailglo_*_linux_amd64.tar.gz mailglo
sudo install mailglo /usr/local/bin/
```

The macOS binaries are signed with a Developer ID and notarized by Apple.

**From source** (Go 1.26 or later):

```sh
go install github.com/mailglo/cli/cmd/mailglo@latest
```

## Authenticate

Create an API token in your Mailglo workspace and export it:

```sh
export MAILGLO_TOKEN=...
```

Treat the token like a password. `mailglo` sends it only over https, or over plain
http to `localhost`, and never prints it.

| Variable        | Purpose                                                         |
| --------------- | --------------------------------------------------------------- |
| `MAILGLO_TOKEN` | API token. Operations that need no sign-in work without it.    |
| `MAILGLO_URL`   | Server URL. Default `https://mailglo.com`.                      |

`--server URL`, given before the command, overrides `MAILGLO_URL`.

## Use

```sh
mailglo operations                    # list every operation the server offers
mailglo help <operation>              # show its flags and body fields
mailglo <operation> [--param value]... [--body JSON|@file|-]
mailglo version
```

Each operation parameter becomes a flag with the same name (`--name value` or
`--name=value`). Path, query and header parameters are all flags. `--body` takes a
JSON request body inline, from a file (`@body.json`) or from standard input (`-`).

Examples (run `mailglo operations` for the full list):

```sh
mailglo get-health
mailglo get-me
mailglo help list-domains
mailglo list-domains --mspId MSP_ID --status active
mailglo add-domains --mspId MSP_ID --clientId CLIENT_ID --body '{"names":["example.com"]}'
echo '{"names":["example.org"]}' | mailglo add-domains --mspId MSP_ID --clientId CLIENT_ID --body -
```

A successful response goes to standard output. An error response goes to standard
error. When a list is cut into pages, `mailglo` says so on standard error and gives
the `--cursor` value for the next page.

## Exit codes

| Code | Meaning                                                   |
| ---- | --------------------------------------------------------- |
| 0    | Success                                                   |
| 1    | Other error, such as a rejected request (HTTP 400 or 422) |
| 2    | Usage error: unknown operation, missing flag, bad body    |
| 3    | Not signed in, or not allowed (HTTP 401 or 403)           |
| 4    | Not found (HTTP 404 or 410)                               |
| 5    | Conflict or stale state (HTTP 409 or 402)                 |
| 6    | Rate limit, outage or network failure: retry later        |

## API description cache

`mailglo` keeps a copy of each server's API description in your user cache directory
(`~/.cache/mailglo` on Linux, `~/Library/Caches/mailglo` on macOS,
`%LocalAppData%\mailglo` on Windows). Every run asks the server whether the copy is
current. When the server cannot be reached, `mailglo` uses the cached copy and prints
a warning. With no cached copy it exits with code 6. Delete the directory to reset
it.

## Build and test

```sh
go vet ./...
go test ./...
go build -o mailglo ./cmd/mailglo
```

The tests run the CLI as a separate process against local test servers.

## Security

Report vulnerabilities to security@mailglo.com. See [SECURITY.md](SECURITY.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
