# Security Policy

## Supported Versions

Open Streamer is under active development. Security fixes land on `main` and
the latest tagged release. Older releases are not backported — please upgrade
to the latest release.

| Version | Supported          |
| ------- | ------------------ |
| 4.x     | :white_check_mark: |
| < 4.0   | :x:                |

## Reporting a Vulnerability

Please report security vulnerabilities **privately**. Do **not** open a
public issue, pull request, or discussion for a suspected vulnerability.

Use GitHub's private vulnerability reporting:

1. Open the repository's **Security** tab.
2. Click **Report a vulnerability**.
3. Describe the issue with as much detail as possible — affected version or
   commit, reproduction steps, impact, and any suggested fix.

We aim to acknowledge a report within **5 business days** and to share a
remediation timeline after triage. When a fix ships we publish a security
advisory and credit the reporter, unless you ask to remain anonymous.

## Where secrets are stored

Every stream secret (the credentials inside input and push URLs, stream keys,
SRT passphrases) is stored in clear in the one YAML config file, because the
server must use them; nothing else holds them. The file's protection is the
operating system's: on Unix the server refuses a config file on which its group
or others have any permission, and writes it 0600, so whoever can read that
file, the service user and root, reads the secrets. Through the API nobody
reads them: every response is masked for every role, the config editor
included, and a write that sends the mask back keeps the stored value. The
API users' passwords are stored as salted PBKDF2-SHA256 hashes (600 000
iterations) in the same file, and an editor can read those hashes through
`GET /api/v1/config`; masking them, and encrypting secrets at rest, are
backlog items.

## Scope

In scope: the server, the native transcoder, and the ingest and publish
pipelines in this repository.

Out of scope: vulnerabilities in third-party dependencies (report those to
the upstream project), and issues that require access the operator already
controls — the host filesystem, or the management HTTP API, which is expected
to be deployed on a trusted network behind the operator's own authenticated
reverse proxy.
