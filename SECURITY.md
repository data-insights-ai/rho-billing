# Security

## Reporting a vulnerability

Report suspected vulnerabilities to **security@data-insights.ai**. Do not open
a public issue for a security report.

Include the affected version or commit, what an attacker can do, and the
smallest reproduction you have. We acknowledge reports within three working
days and will tell you whether we consider the report in scope and when we
expect a fix.

Please do not run tests against anyone else's deployment, and do not include
real customer data or live credentials in a report.

## Scope

This is a library. It has no network listener and no process of its own: the
host application owns HTTP, authentication and secret storage. Findings in this
repository therefore look like incorrect authorization boundaries, money
arithmetic that can be made to lose or mint value, identity that can be made to
alias across tenants or environments, or data that leaks between accounts.

In scope:

- A caller reaching another account's data through a documented public API.
- Credit, usage or payment arithmetic that can be driven to an incorrect
  balance — overspend, double grant, double fulfillment, lost refund
  provenance.
- Provider identity aliasing across merchant or environment boundaries.
- Webhook signature verification accepting a payload it should reject.
- Secrets or customer payloads reaching logs, errors or diagnostics.

Out of scope:

- Anything requiring the host to have already skipped authorization. The
  library assumes the host authorized the caller; account IDs are not
  credentials.
- Vulnerabilities in the payment provider itself. Report those to the
  provider.
- Denial of service through resource exhaustion by a caller the host has
  already authorized.

## Handling credentials

Credentials never belong in source, commits, documentation, fixtures, command
arguments or logs. Tests read provider credentials from a key file with mode
0600, referenced by environment variable, and skip when it is absent.

Diagnostics are sanitized before they are returned or logged: response bodies
are bounded and reflected keys are redacted, so a provider error cannot carry a
secret into a log line.

## Supported versions

Pre-1.0. Only the latest released version receives fixes.
