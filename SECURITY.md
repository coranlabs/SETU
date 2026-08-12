# Security Policy

## Reporting a vulnerability

Please report security issues **privately**. Do not open a public issue.

Use GitHub's [private vulnerability reporting](https://docs.github.com/code-security/security-advisories/guidance-on-reporting-and-writing/privately-reporting-a-security-vulnerability)
on this repository, or email **contact@coranlabs.com**.

Please include a description of the issue, the steps or configuration needed to reproduce it, the
affected version, and the impact as you see it. We will acknowledge your report within five working
days and keep you informed while we investigate. We are happy to credit you when the fix is
published, unless you prefer otherwise.

## Supported versions

The most recent release receives security fixes.

## Scope and known limitations

SETU sits on the signalling path of a mobile core network. Some deployment defaults favour
lab usability, and you should change them before any deployment that matters:

- **TLS verification** can be disabled per connector (`insecure: true`) for self-signed certificates.
  Do not use it outside a lab.
- **Administrative endpoints** (`/metrics`, `/healthz`) are unauthenticated. Bind them to a
  management interface or place them behind a proxy.
- **Signalling interfaces** (Diameter, the SMS ingest) carry no application-layer authentication of
  their own. Restrict them at the network layer to the peers that should reach them.
- **Subscriber key material** passes through the process when a core delegates vector generation to
  it. Treat the host, its logs and its memory accordingly.

Configuration weaknesses in the examples are in scope: if a default in this repository would lead a
reasonable operator into an insecure deployment, we want to hear about it.
