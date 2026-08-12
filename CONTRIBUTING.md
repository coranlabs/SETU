# Contributing to SETU

Thanks for your interest. Connectors for additional 5G cores are the most valuable
contribution you can make — they are what turn a working bridge into a portable one.

## Developer Certificate of Origin

This project uses the [Developer Certificate of Origin](https://developercertificate.org/)
rather than a contributor licence agreement. Sign off every commit:

```bash
git commit -s -m "connectors/open5gs: initial policy backend"
```

The sign-off certifies that you wrote the contribution, or have the right to submit it under the
project's licence. Contributions are accepted under Apache-2.0.

Add the SPDX header to new files:

```go
// SPDX-FileCopyrightText: 2026 <copyright holder>
// SPDX-License-Identifier: Apache-2.0
```

## Before you open a pull request

```bash
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

The tree builds against the standard library only, offline. Please keep it that way: a dependency
has to earn its place, and none has so far.

## Adding a core connector

1. Create `connectors/<core>/` implementing `api.CoreConnector`.
2. Declare `Capabilities()` honestly — the platform adapts to what a core supports. Claiming a
   capability you do not have produces confusing runtime failures rather than a clean fallback.
3. Put core-specific constants in `dialects/<core>.json`, not in code.
4. Add tests against a fake core using `httptest`; see `connectors/sdcore/connector_test.go`.
5. Register the connector in `cmd/setu/main.go`.

## Style

Comments explain *why*, not *what*. Cite the specification clause when behaviour follows one, and
note gotchas that would otherwise cost someone an afternoon. Skip comments that restate the code.

## Reporting bugs

Include the SETU version, the core and IMS software in use, the relevant configuration with secrets
removed, and log excerpts around the failure. Wire-level traces help enormously.

For security issues, follow [SECURITY.md](SECURITY.md) instead of opening an issue.
