---
title: "API response validation: the e2e tier checked against the served OpenAPI document"
status: validated
track: core
depends_on:
  - specs/013-api.md
  - specs/014-test-stubs-and-tiers.md
affects: [test/e2e/, go.mod, .lateregate.yaml]
effort: medium
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# API response validation

## Overview

[[013-api]] generates `api/openapi.yaml` from the route table and serves
the same document at `GET /openapi.json`, and a gate holds the committed
file equal to a fresh generation. That proves the document and the
declaration agree. It does not prove the document and the wire agree: a
handler that writes a field the schema does not name, omits one it
requires, or answers a status the document does not list, passes every
test in the tree today.

The half that would catch it is the second half of [[013-api]]'s
criterion 13, carried here verbatim: every request and every response of
the e2e tier validates against the served document. It was split out of
that spec when Arca closed at v0.1.7 rather than built, because it is the
one open item that cannot be written without adding a dependency.

## Why this was split

Validating a request and a response against an OpenAPI 3.1 document needs
an OpenAPI 3.1 request and response validator on the test build list.
That is a new module in `go.mod` and a new row for whoever reads the
dependency list, and [[001-architecture]]'s invariant 9 makes a
dependency a decision recorded in `.lateregate.yaml` rather than an
import somebody adds. `internal/apidocs` builds the document as Go values
precisely so the server links no YAML parser, and `tools/apidoc` is the
one place a YAML encoder is reached; a validator is the second such
place, and choosing it is choosing which library reads the schema
dialect the generator emits. That is a decision with a review, not an
hour's work before a tag, and the rest of [[013-api]] is proved and
shipping. So the spec closed on what shipped and the validator became
this one, at `validated`, to be dispatched when the dependency is taken.

## Design

The e2e tier of [[014-test-stubs-and-tiers]] drives `arcad` as a process
over HTTP. Every request it makes and every response it reads passes
through one client, so the seam is one place.

| Piece | Where |
|---|---|
| the document | `GET /openapi.json` from the installation under test, read once at harness start, so the tier validates against what this build serves and not against the committed file |
| the validator | one module on the test build list, admitted by a row in `.lateregate.yaml`'s `depcheck` with the reason it is there, and reached from the test tree alone: `./cmd/arcad`'s build list does not change |
| the seam | the tier's HTTP client, wrapped so each round trip is validated before the test sees the response; a request or a response the document does not describe fails the test that made it, naming the route, the status and the member |
| the exemptions | the object byte routes, whose bodies are opaque octet streams the document describes by content type and not by schema, and the presigned URLs, which are the bucket's answers and not this API's |

A validator that cannot read the dialect `internal/apidocs` emits is a
reason to change the generator's output, not a reason to weaken the
check: the document a consumer's generator reads is the document the
validator reads.

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | Every request and every response of the e2e tier validates against the served document | the tier's client, with one test that fails when a handler writes a member the document does not name |
| 2 | The validator is on the test build list alone: `go list -deps ./cmd/arcad` reaches it in no build | the `depcheck` gate, whose `cmd/arcad` row does not admit it |
| 3 | Each route the check exempts is named with the reason, and no route is exempt by silence | the exemption list, read by a test against [[013-api]]'s route table |
