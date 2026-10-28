# Coverage policy

`make check` (and CI) require **100% statement coverage** of `internal/`,
measured with the race detector on (`.testcoverage.yml`).

- The only exclusion is `cmd/ex-runner`: it wires the CLI to the real process
  (argv, signals, opening the browser) and is exercised by the binary itself.
- Code that talks to the outside world (the ex server, git, the agent CLIs,
  dev servers) is tested against real local stand-ins: `httptest` servers,
  bare git repos and `git http-backend`, fake `claude`/`codex` executables.
- Error arms that are hard to reach go through small fault-injection seams
  (package-level function variables swapped by tests). `// coverage-ignore`
  is not used.
- Tests that need a Unix shell are `//go:build !windows`; Windows builds are
  vetted in CI.
- The Secure Enclave shim (`internal/devicekey/se_darwin.go`, `darwin && cgo`)
  is C behind a branch-free Go wrapper: errors come back as text and every
  decision is made in portable Go, tested through a fake enclave. On macOS a
  test calls the real shim (CI's macOS VMs have no enclave and take the
  fallback). The Linux gate never compiles it; the C itself is outside Go
  coverage.
