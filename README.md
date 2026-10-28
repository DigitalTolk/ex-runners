# ex-runner

`ex-runner` runs your [ex](https://github.com/DigitalTolk/ex) agents on your own
computer. Use it for agents that run on **Claude Code** or **Codex**. Agents
that run on AWS Bedrock run on the ex server and don't need it.

It ships as one self-contained program, with no runtime to install, for
macOS, Linux and Windows on both Intel/AMD and ARM.

The runner connects outward to your ex server and asks it for work. It never
listens for incoming connections, except on `127.0.0.1` for the few seconds
that `ex-runner login` takes. Every agent run uses the access of the person who
asked for it, and the run ends with that person's request.

## Install

Download the archive for your system from the
[Releases](https://github.com/DigitalTolk/ex-runners/releases) page, unpack it,
and put `ex-runner` on your `PATH`. Each archive's checksum is in `SHA256SUMS`.

You also need the agent CLI you want to use, installed and signed in on this
computer:

- **Claude Code:** `claude`, signed in.
- **Codex:** `codex`, signed in with `codex login`.

## Connect this computer

```sh
ex-runner login https://ex.example.com
```

1. Your browser opens on ex.
2. Click **Connect**.
3. The browser hands this computer a one-time code over `127.0.0.1`.

The runner exchanges that code for its own token. Exchanging the code also
needs a PKCE secret that never leaves this computer, so a code copied out of
the browser is useless on its own.

This computer now appears on the **Runners** page in ex. Removing it there
disconnects it right away.

Login also makes a key that belongs to this computer and registers its public
half with the token. Every request the runner makes is signed with it, so a
copied `credentials.json` is useless anywhere else. On a Mac the key lives in
the Secure Enclave: only a wrapped copy, usable by this Mac alone, is saved,
and nothing is added to the Keychain. Elsewhere it is a key file only you can
read. A runner signed in with an older ex-runner keeps working unlocked; run
`ex-runner login` again to lock it.

## Run it

```sh
ex-runner start                 # in a terminal; Ctrl-C to stop
```

If the server can't be reached when ex-runner starts (for example, the laptop
logged in before Wi-Fi connected), it keeps retrying, waiting up to a minute
between tries.

The first Ctrl-C stops the agents running here and fails their runs with a
clear message in chat. A second Ctrl-C quits immediately.

Tokens last 7 days. A running ex-runner renews its token automatically during
the last 3 days before it expires.

## Other commands

```sh
ex-runner status          # server, account, machine name, token expiry
ex-runner status --json   # the same plus whether a runner is running, as JSON (never the token or key)
ex-runner logout          # disconnect this computer and delete its token
ex-runner login --no-browser <server-url>   # print the connect link instead of opening it
```

The ex desktop app uses `--no-browser` and `status --json` when you set up a
runner from its Runners tab.

## Files

All files live in `$EX_RUNNER_HOME`, which defaults to `~/.ex-runner`:

| Path               | What                                                    |
| ------------------ | ------------------------------------------------------- |
| `credentials.json` | Server, account and runner token (readable only by you) |
| `device-key.json`  | This computer's key (Secure Enclave blob or software key; readable only by you) |
| `state/`           | Runner ID, warm-session pins, per-thread work folders   |
| `runner.pid`       | Stops a second runner from starting on this computer    |

Coding tasks put their repository checkouts in `~/ex-workspace`. Set
`EX_WORKSPACE_ROOT` to use a different folder.

## Development

```sh
make check       # what CI runs: gofmt + golangci-lint, cross-OS vet, race tests, 100% gate
make test        # go test -race ./...
make cover       # 100% statement gate (see .testcoverage.yml and COVERAGE.md)
make vet         # also vets the Windows, Linux and macOS builds
make build       # ./dist/ex-runner
./dist/ex-runner login http://localhost:5173
./dist/ex-runner start
```

Point `EX_RUNNER_HOME` at a scratch folder to keep a development runner apart
from your real one.

### Releasing

Push a `vX.Y.Z` tag. The release workflow runs the checks, builds one binary
per platform into `ex-runner-X.Y.Z-<os>-<arch>.tar.gz`, and attaches them to a
GitHub Release with a `SHA256SUMS` file. The macOS binaries use cgo for the
Secure Enclave and build on a macOS runner (`make release-darwin`); Linux and
Windows are static binaries built on Linux (`make release-other`). On a Mac,
`make release` builds all six.

### Code layout

| Path                     | What                                                                   |
| ------------------------ | ---------------------------------------------------------------------- |
| `cmd/ex-runner`          | Process entry: binds the CLI to argv, signals and the browser          |
| `internal/cli`           | `login` (PKCE + localhost callback), `start`, `status`, `logout`        |
| `internal/runner`        | The loop: register, then long-poll for work, run it, send heartbeats   |
| `internal/harness`       | Driving Claude Code and Codex, and reading their event streams         |
| `internal/mcp`           | The per-run MCP server agents use to call ex tools (`ex-runner mcp-server`) |
| `internal/workspace`     | Coding-task checkouts, merge requests and dev servers                   |
| `internal/taskpolicy`    | Which commands a coding task may run without asking                    |
| `internal/secretpaths`   | Credential locations agents never reach without a human (or at all, for Codex) |
| `internal/connectors`, `internal/connectordocs` | Connector credentials, docs sync and lookup         |
| `internal/devicekey`     | This computer's key (Secure Enclave or software) and request proofs     |
