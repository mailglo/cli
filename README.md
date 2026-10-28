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

Treat the token like a password. `mailglo` sends it only to the server you chose,
over https, or over plain http to `localhost` or a literal loopback address such as
`127.0.0.1`. Plain http to this machine connects directly, never through a proxy.
No request follows a redirect to another server, and the token is never printed.

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
`--name=value`), whether it goes in the path, the query, a header or a cookie.

- When two parameters share a name, give the location: `--path.id 1 --query.id 2`.
- Repeat the flag for a list parameter: `--tag a --tag b`.
- `--body` takes the JSON request body inline, from a file (`@body.json`) or from
  standard input (`-`). If the operation has a parameter called `body`, use
  `--request-body` for the request body instead.

`mailglo help <operation>` shows the flags, whether each one is required or
repeatable, and the body's fields and media type. Operation names are the API's
operation IDs in kebab case, such as `list-domains`.

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

## Supported API descriptions

`mailglo` is built for the Mailglo API description, not for every OpenAPI document.
It accepts this subset:

- **Document**: OpenAPI 3.0 or 3.1, served as JSON, with a `paths` object.
- **References**: local `$ref`s only (`#/...`), read as JSON Pointers with
  percent-decoding, `~0`, `~1` and array indices (`0` or `[1-9][0-9]*`). Beside
  `$ref`, only `summary` and `description` are allowed.
- **Paths**: absolute (`/...`), without `?`, `#`, spaces or backslashes. Every
  `{variable}` has a path parameter and every path parameter is in the path.
- **Servers**: the first `servers` entry of the operation, else of the path, else of
  the document. Each `{variable}` is replaced once by its default, and the result
  must be at most 2048 bytes. A relative URL resolves against
  the URL the description came from. Only its path is used: requests always go to
  the server you selected. With no `servers`, the base path is `/api/v1`.
- **Parameters**: scalar or array schemas, each declared once per list, with no `=`
  in the name.
  - path: `simple`, `label` or `matrix`
  - query: `form`; arrays also `spaceDelimited` or `pipeDelimited`
  - header: `simple`
  - cookie: `form`, single values only

  Not supported: `content` instead of `schema`, object schemas, `allOf`, `oneOf`,
  `anyOf` or `not` in a parameter schema, `deepObject`, `allowReserved`, other
  locations, and header parameters the CLI or HTTP sets itself (`Accept`,
  `Authorization`, `Content-Type`, `Cookie`, `Host`, `User-Agent` and other
  connection headers).
- **Request bodies**: `application/json`, any `*/*+json` type, or
  `application/octet-stream` (sent as raw bytes from `--body @file` or `-`).

### Threat model

The API description comes from the server you chose, over https or from your own
machine, so `mailglo` trusts it to describe that server's API. It does not trust it
with anything else: requests, the token and redirects stay on the selected server's
origin, and text from the description is escaped before it reaches your terminal.

A description outside the subset, or a hostile one, may not work, but it cannot make
`mailglo` misbehave. Work and memory are bounded: the description is at most 64 MiB,
parsing has a fixed step budget, and server URLs are capped. Anything the CLI cannot
handle is refused with a message that names it.

A document outside the subset is refused. An operation outside it still appears in
`mailglo operations` and `mailglo help` with an `unsupported` field that names the
construct, and running it exits with code 2.

Command names are operationIds in kebab case. An operation without an operationId
is named after its method and path, such as `get-health`. When names clash, or an
operation would be named `help`, `operations` or `version`, the method is appended
(`version-get`), then a number.

## Exit codes

| Code | Meaning                                                                          |
| ---- | -------------------------------------------------------------------------------- |
| 0    | Success                                                                          |
| 1    | Other error: a rejected request (HTTP 400 or 422), output that cannot be written |
| 2    | Usage error: unknown operation, missing flag, bad body                           |
| 3    | Not signed in, or not allowed (HTTP 401 or 403)                                  |
| 4    | Not found (HTTP 404 or 410)                                                      |
| 5    | Conflict or stale state (HTTP 409 or 402)                                        |
| 6    | Rate limit, outage or network failure: retry later                               |

## API description cache

`mailglo` keeps a copy of each server's API description in your user cache directory
(`~/.cache/mailglo` on Linux, `~/Library/Caches/mailglo` on macOS,
`%LocalAppData%\mailglo` on Windows). Every run asks the server whether the copy is
current. When the server cannot be reached, `mailglo` uses the cached copy and prints
a warning. With no cached copy it exits with code 6. On macOS and Linux it uses the
directory only if it belongs to you and no one else can write to it. Delete the
directory to reset
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
