# Security policy

## Report a vulnerability

E-mail security@mailglo.com. Do not open a public issue.

Include the affected version (`mailglo version`), the steps to reproduce and the
impact you expect. We reply to confirm we have the report and tell you when a fix is
released.

## Supported versions

Security fixes go into the latest release. Upgrade to it before you report.

## Scope

This repository covers the `mailglo` command-line client. Report issues in the
Mailglo service itself to the same address.

## Threat model

`mailglo` reads its commands from the API description of the server you select,
over https or from this machine. It trusts that description to describe the
server's API, and nothing more:

- Requests, the API token and redirects stay on the selected server's scheme, host
  and port. The token is sent over plain http only to `localhost` or a literal
  loopback address. Over plain http to this machine, both the description and the
  requests connect directly, never through a proxy.
- Text from the description and from responses is escaped before it reaches the
  terminal.
- The cached description is used only when the cache directory and file belong to
  you and no one else can write to them.
- A description outside the supported subset (see README.md), or a hostile one,
  must not cause unbounded memory or CPU use, and must be refused rather than
  misinterpreted. Supporting it is not a goal. The description is read with
  kin-openapi, capped at 16 MiB and loaded under a deadline.

Reports that break these properties are in scope.
