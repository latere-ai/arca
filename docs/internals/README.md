# Internals

For people changing Arca. The pages beside this one describe how the
server is built and tested; the user-facing pages one level up describe
what it does. The design records, with the reasoning and the acceptance
criteria behind each module, are in [`specs/`](../../specs/README.md), and
[`CONTRIBUTING.md`](../../CONTRIBUTING.md) is how to send a change.

| Page | |
|---|---|
| [Architecture](architecture.md) | the packages, the two stores and the order they are written in, the request path, uploads, workspaces, the event log and the usage ledger, and the reconciler |
| [Testing](testing.md) | the unit, store, e2e, and conformance tiers, the stubs, the deploy and threat model tests, and how to run each |
| [Releasing](releasing.md) | the changelog rule, cutting a tag, and what the release workflow builds, signs, proves, and publishes |
| [Migrating from Drive](drive-migration.md) | the one-time procedure that moved Latere's predecessor service onto Arca, kept as the record of how `tools/migrate-drive` and `tools/move-objects` are used |
