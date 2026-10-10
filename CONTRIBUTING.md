# Contributing

Small, focused pull requests are welcome. Explain the problem, expected behavior,
and verification performed. Contributions are provided under the
[Apache-2.0 license](LICENSE). See [MAINTAINERS.md](MAINTAINERS.md) for support.

Use Go 1.25 and follow the [development instructions](README.md#development-and-tests).
Run `gofmt`, `go mod verify`, the race tests, and `go vet`. Changes affecting the
webhook protocol or reconciliation also require the actual-controller test in
`scripts/e2e.py`. Keep chart and controller versions pinned independently.

Preserve ownership boundaries, domain and zone filters, sidecar dry-run, and
selected-value deletion. Add regression coverage for recovery after partial
writes and retries when changing mutations.

Keep API tokens, kubeconfigs, customer names, and private test reports out of Git.
Live tests require an explicitly configured disposable delegated zone and scoped
credentials; they must never run automatically on untrusted pull requests.
See [SECURITY.md](SECURITY.md) for private vulnerability reports.

Use concise commit subjects such as `fix: preserve ownership on retry` or
`test: cover filtered zones`. Update the README for user-visible changes.

