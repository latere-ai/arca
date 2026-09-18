## What this changes

<!-- What behaviour is different after this, for whoever stores an object, runs arcad, or builds on the packages. -->

## Why

<!-- The problem it solves. Link the issue or the spec if there is one. -->

## Checklist

- [ ] `make` passes locally.
- [ ] A bug fix carries a test that fails without it.
- [ ] A change to the `/v1` API, a configuration variable, a webhook
      payload, an event payload, or the bucket key layout is reflected in
      its spec and in `docs/`.
- [ ] A change to an exported package keeps every existing call site compiling,
      or the CHANGELOG names the break.
