---
title: "Shares and links: grants and the permission ladder, public links, what a caller sees shared with them"
status: testing
track: core
depends_on:
  - specs/004-metadata-store.md
  - specs/005-files.md
  - specs/006-identity.md
affects: [space/, authorizer/, internal/shares/, internal/api/, internal/store/, docs/]
effort: large
created: 2026-09-18
updated: 2026-09-18
author: changkun
---

# Shares and links

## Overview

A space belongs to one subject, and most of what a space holds is meant
for exactly one reader. The rest is not. A person hands a directory to a
colleague, a service is given write access to one prefix so it can drop
artifacts there, an organization is given read access to a release tree,
and a browser with no token at all is handed one file by URL. This spec
is the record Arca keeps of all four: the grant.

A grant says that one grantee holds one permission on one subtree of one
space. Grants are data, not decisions. Arca stores them, lists them,
revokes them, and offers them to the decision [[006-identity]] makes; it
never reads a claim to work out who a grantee is. An organization is a
subject the authorizer names, exactly like a person or a service, so Arca
holds no group table, no membership cache, and no org claim.

Public links are the same record with no grantee subject and a token
instead. The token is the capability: whoever holds it reads the subtree
it names, carrying no token of their own. That is the one place in `/v1`
where the verification half of invariant 5 of [[001-architecture]] does
not apply, and this spec states the exception in full so
[[015-security-and-threat-model]] can reason about it. The asking half
still holds: a redeemed token still asks `link.read`.

## Current state

Built and in the tree on 2026-09-18, phase 4 of [[019-migration-from-drive]].
Migration `0003_shares.up.sql` creates the table, the number
[[004-metadata-store]]'s ownership table gives this spec;
`internal/store/shares.go` and `links.go` hold the query set;
`internal/shares` holds the eleven handlers, the two lookups
[[006-identity]] reads a decision through, and the token; `internal/api`
gains the `Shares` seam its route table dispatches through, and the eleven
rows of [[013-api]] that belong here; `cmd/arcad` binds one query set to
both readers. The commits are `850e0b6` (the migration and the queries),
`91ad501` (the grants), `16a35a7` (the links) and `2976467` (the wiring and
the e2e tier). The gate passes with all fifteen gates on at each of them.

What arrived from the service Arca replaces is
`drive/internal/handler/shares.go` (create, list, what is shared with the
caller, revoke), `drive/internal/handler/links.go` (the three token routes),
the ladder of `drive/internal/handler/authz.go`, which is now the owner
policy's, and migrations `000002`, `000010`, `000013` and `000017`, which
become the one table above. The share-request routes, the approval statuses,
the resolution columns, and the grantee kinds read from a claim do not
arrive; three CHECK constraints hold the narrowing in the schema.

Two defects of the code this port carries over are fixed here, each with a
test that fails without the fix:

- `drive/internal/handler/shares.go:358`, the listing of a space's shares,
  answers the `token` column to every caller that may list. A token is the
  capability, so a listing that carries one hands the capability to
  everyone who may read the listing; only the grantee's own listing stripped
  it (`shares.go:397`). Here the token is answered once, by the create, and
  no other shape carries it (`TestMintingALinkAnswersTheTokenOnce`).
- `drive/internal/handler/authz.go:166`, the grant step, admits a `public`
  grant for any authenticated caller asking to read. A token grant is the
  link step's, and answering it in the grant step gives every signed-in
  caller whatever the public holds without a token. Here only a `subject`
  grant answers that step (`TestWhatTheGrantStepDoesNotAdmit`).

Criteria 1, 3, 4, 5, 5b, 6, 7, 8, 9 and 11 have passing tests; criterion 2
is proved per route in `internal/shares` and closes with
[[017-conformance-suite]]'s row per action. Criterion 10 is open: the append
is proved against the seam, and the tail that reads it back arrives with
[[010-events-and-reaper]]. The spec stays at `testing` until both close.

Two seams are declared here and bound by a later spec. `Ledger` is the log
of [[010-events-and-reaper]] as a mutation writes it, with a no-op default,
and the append runs inside the mutation's own transaction: a change to who
may act on a space is not a notification that may go missing. `ObjectReader`
is the read path of [[005-files]], which the third link route serves an
object through; a build that binds none answers `not_implemented` from that
one route and serves the other two.

What the implementation decided, where this spec was silent:

- The route is `GET /v1/shares/with-me`, which is [[013-api]]'s grammar and
  the rename [[019-migration-from-drive]] names. The table above said
  `/v1/shared-with-me`, the predecessor's path, and is corrected.
- `POST /v1/shares` grants to a subject and takes no kind; a token grant is
  minted at `POST /v1/shares/links`, which takes `kind`, `link` or `public`.
  A `permission` on that route is `read` or absent, and anything else is
  `link_read_only`, which is where criterion 4 is answered.
- The two kinds are read and revoked at their own routes, because they ask
  two different actions. A link reached through `/v1/shares/{id}`, and a
  subject grant reached through `/v1/shares/links/{id}`, are each the answer
  a grant that is not there gets.
- A deny of `link.read` is answered `not_found` rather than `forbidden`, so
  an installation that turned public reading off is indistinguishable from a
  token that never existed.
- An `expires_at` already past is `invalid_field`. A grant that grants
  nothing from the instant it is written is a caller's mistake, not a state
  worth storing.
- The event detail carries the grant's id beside the three fields the table
  below names, so a consumer can pair a create with its revoke.
- A public grant's two writes are ordered so that a failure grants less than
  the caller asked for and never more: the row is marked inside the
  transaction and the bucket is stamped after it commits, and a revoke
  clears the bucket first. A bucket that will not answer is
  `storage_unavailable`.
- The prefix a grant carries is normalised: a trailing slash is not part of
  a subtree's name, and the whole of a plane is a prefix, since a path in no
  plane is a path this server does not serve.
- A grant's listing is keyset paginated on the id as text, and the cursor is
  compared in the same domain the order is taken in, so a walk resumes where
  it stopped whatever collation the database was created with.
- `internal/api` gained two pieces of [[013-api]]'s frame that a route with
  a body needs and phase 2 had no caller for: the strict JSON decoder in the
  error envelope, and the `{owner}` rule with its one alias.

## Design

### The grant

| Field | Meaning |
|---|---|
| `id` | the grant's identity, the handle a revoke names |
| `owner` | the space the grant is on, the subject `<issuer>\|<sub>` |
| `path_prefix` | the subtree the grant covers, a path inside the space |
| `grantee_kind` | `subject`, `link`, or `public` |
| `grantee` | the grantee subject, set when `grantee_kind` is `subject` |
| `token` | the capability, set when `grantee_kind` is `link` or `public` |
| `permission` | `read`, `write`, or `manage` |
| `status` | `active` or `revoked` |
| `expires_at` | when the grant stops counting, null for no expiry |
| `created_by` | the subject that created the grant |
| `created_at` | when |

[[004-metadata-store]] owns the table and its indexes. The columns above
are the ones behaviour in this spec depends on.

A grant covers a subtree, so a grant on a workspace root is a share of
that workspace and [[009-workspaces]] needs no grant table of its own.
`path_prefix` is matched by path segment: `files/reports` covers
`files/reports` and `files/reports/q3.pdf` and does not cover
`files/reports-archive`.

### The permission ladder

```
read  <  write  <  manage
```

| Permission | Holds |
|---|---|
| `read` | list and download inside the subtree |
| `write` | read, plus put, move, and delete inside the subtree |
| `manage` | write, plus create and revoke grants on the subtree |

Each grade holds every grade below it. The ladder is one ordering used by
every plane, so a `write` grant on `workspaces/build/` is what lets a
sandbox attach read-write ([[009-workspaces]]) and a `read` grant is what
lets it attach read-only.

`manage` exists so a space owner can delegate sharing without delegating
ownership. Whether a given `manage` holder may mint a given grant is not
decided here: the `share.create` question carries the permission being
minted and the grantee, so the deciding side sees an attempted
escalation and can refuse it. The built-in owner policy's answer is
[[006-identity]]'s.

### Grantee kinds

| Kind | Grantee | How access is presented |
|---|---|---|
| `subject` | a person, a service, or an organization, all one subject string the authorizer names | a token from a listed issuer |
| `link` | nobody; the token is the grantee | the token in the URL |
| `public` | nobody; the token is the grantee, and the grant is advertised as public | the token in the URL, or a direct object URL |

Arca does not interpret the subject. `<issuer>|<sub>` for a person, for a
service account, and for an organization are the same shape and take the
same code path. An installation whose authorizer names organizations gets
organization sharing with no schema in Arca.

`link` and `public` grants carry `read` only. A create that asks for
`write` or `manage` on a token grant is a request error.

### The lookup

One query, and no decision:

```go
// Covering returns the active, unexpired grants on owner's space whose
// prefix covers path, highest permission first.
func (s Shares) Covering(ctx context.Context, q Querier, owner, path string) ([]Grant, error)
```

One indexed statement fetches every candidate grant for the space and the
prefixes of `path`, and the filter on `status` and `expires_at` is in the
statement. [[006-identity]] owns what a decision does with the result.
Nothing in `internal/shares` reads a claim.

The query lives in `internal/store` with the table and the rest of the query
set ([[004-metadata-store]]), and `internal/shares` binds it to the two
seams [[006-identity]] declares:

```go
// Grants answers the grant step of the owner policy's flowchart, and Links
// the link step. Both are read only and take no transaction.
func Grants(db Database, queries store.Shares) auth.GrantLookup
func Links(db Database, queries store.Shares) auth.LinkResolver
```

Only a `subject` grant answers the grant step. A token grant is the link
step's, one branch further down the flowchart, and reading one in the grant
step would hand every caller with a token of their own whatever the public
holds.

### Routes

| Method | Path | Action | Resource fields carried |
|---|---|---|---|
| POST | `/v1/shares` | `share.create` | `owner`, `path`, `permission`, `grantee` |
| GET | `/v1/shares` | `share.list` | `owner`, `path` |
| GET | `/v1/shares/{id}` | `share.read` | `id`, `owner`, `path`, `grantee`, `permission` |
| DELETE | `/v1/shares/{id}` | `share.revoke` | `id`, `owner`, `path`, `grantee`, `permission` |
| GET | `/v1/shares/with-me` | `share.list` | `grantee`, the caller's own subject |
| POST | `/v1/shares/links` | `link.create` | `owner`, `path` |
| GET | `/v1/shares/links` | `link.read` | `owner`, `path` |
| DELETE | `/v1/shares/links/{id}` | `link.revoke` | `id`, `owner`, `path` |

Every route above is authorized per [[006-identity]] with the action
named. The grant's `path_prefix` is carried to the authorizer as the
resource field `path`, which is the name [[006-identity]] fixes for every
kind. The `Link` kind has no `list` action in that vocabulary, so a
listing of a space's links asks `link.read` for the prefix it lists.
[[013-api]] owns the status codes, the pagination, and the encoding of
`owner`.

A create:

```json
POST /v1/shares
{
  "owner": "me",
  "path_prefix": "files/reports",
  "grantee": "https://issuer.example|c2f1a0e4-...",
  "permission": "write",
  "expires_at": "2026-10-18T00:00:00Z"
}
```

```json
201
{
  "id": "0f1b...",
  "owner": "https://issuer.example|9ab3...",
  "path_prefix": "files/reports",
  "grantee_kind": "subject",
  "grantee": "https://issuer.example|c2f1a0e4-...",
  "permission": "write",
  "status": "active",
  "expires_at": "2026-10-18T00:00:00Z",
  "created_at": "2026-09-18T10:02:11Z"
}
```

`GET /v1/shares/with-me` answers the grants whose `grantee` is the
caller's own subject. It is one query on the grantee index and needs no
membership anywhere. An authorizer that narrows a list answers the
`share.list` question with a `filter`, and the handler applies it to its
own query; [[006-identity]] owns the filter's shape.

### Public links

A token grant is minted with 256 bits from `crypto/rand`, rendered
base64url, stored under a unique index. Three routes redeem it, and all
three are the same three routes the service Arca replaces served:

| Method | Path | Answers |
|---|---|---|
| GET | `/v1/shares/links/{token}/meta` | `{"kind","owner","path_prefix"}`, what a viewer needs before it fetches anything |
| GET | `/v1/shares/links/{token}` | a listing of the subtree, paginated |
| GET | `/v1/shares/links/{token}/files/{path...}` | one object, as a redirect to a presigned URL, or inline below `ARCA_INLINE_BYTES` |

These three require no bearer token, and the order they work in is
fixed. The handler resolves the token to its grant first, refuses it
unless `status` is `active` and `expires_at` is in the future, and only
then asks `link.read` per [[006-identity]] with the resolved grant's
`id`, `owner`, and `path` and an empty subject. The token is what
identifies the resource; the question is what an operator can still
refuse, so an installation that wants no public reading denies
`link.read` for the empty subject and every link in the database stops
working at once. The owner policy allows a `link.read` whose token
resolved, which is the branch [[006-identity]]'s diagram draws.

Serving is confined to the grant: only paths the prefix covers, and reads
only, because a token grant carries `read`.

Requiring no bearer token is deliberate: a link that needed one would not
be a link. [[015-security-and-threat-model]] carries the consequences,
which are that the token is a bearer secret, that it must not appear in a
log, and that the only revocation is the row. [[013-api]] records that
these three routes are the unauthenticated part of `/v1`.

The `public` kind differs from `link` in two respects. `meta` says so,
and a `public` grant whose prefix is one object marks that object public
in the bucket: the handler calls `SetPublic` ([[003-object-store]]) on
create and clears it on revoke, and a read then answers `302` to
`ARCA_PUBLIC_CDN_URL` rather than to a presigned URL. Publicity is
therefore a property of the object recorded in its row, derived from the
grant and never from the path: the service Arca replaces read
`files/avatar` and `files/public/**` as public by name, and Arca reads
no such convention. Nothing else depends on the distinction.

There is no HTML page. The service Arca replaces rendered `/s/{token}` as
a branded viewer; Arca serves JSON and bytes, and the hosted platform
that builds on Arca renders whatever page it wants on top of these three
routes.

### Revocation

`DELETE` sets `status` to `revoked` and stamps nothing else. The effect is
immediate and there is no grace window: the next `Covering` call does not
return the grant, and the next redemption of a revoked token is a
not-found. A revoked row is kept so an audit can see the grant existed
([[012-administration]]); the reaper of [[010-events-and-reaper]]
deletes grants that expired long ago.

Expiry needs no sweep to take effect. `Covering` and the token lookup both
filter on `expires_at`, so an expired grant stops granting at the moment
it expires whether or not anything has run.

### Events

Every mutation appends to the log of [[010-events-and-reaper]]:

| Mutation | Action | Detail |
|---|---|---|
| a grant is created | `share_created` | `grantee_kind`, `permission`, `path_prefix` |
| a grant is revoked | `share_revoked` | `grantee_kind`, `path_prefix` |

A link is a grant, so a link create emits `share_created` with
`grantee_kind` `link` rather than an action of its own. The event action
enum is closed and small on purpose; a filter that wants links alone reads
the detail.

### What arrives from Drive

| From | What changes |
|---|---|
| `.archive/003-sharing-acl.md` | the grant, the subtree model, the ladder, the token routes |
| `.archive/017-manage-permission.md` | the `manage` grade and the no-escalation constraints, now carried as resource fields into the `share.create` question instead of decided in the handler |
| `.archive/013` and `drive/internal/handler/links.go` | the three token routes, unchanged in shape; the HTML viewer they fed is gone |
| `drive/internal/handler/shares.go` | create, list, shared-with-me, revoke |
| `drive/internal/handler/authz.go` | replaced. The resolution it performed is [[006-identity]]'s; what survives is `Covering`, a query |
| migration `000010_public_share_tokens` | tokens on public grants, folded into the initial schema of [[004-metadata-store]] |

Owner addressing changes from `(owner_type, owner_id)` with a `u-`/`o-`
prefix to the single subject `<issuer>|<sub>`. The `org` owner alias goes
with it: `me` survives, `org` does not, because it was read from a claim.

Left behind, and named for [[019-migration-from-drive]]:

1. **Share requests and approvals.** `GET /v1/orgs/{org}/share-requests`,
   `POST /v1/orgs/{org}/share-requests/{id}/resolve`, the `pending` and
   `denied` statuses, and the `resolved_by`, `resolved_at`, and
   `resolution_note` columns (`drive/internal/handler/shares.go`,
   `.archive/004-share-approvals.md`). An approval queue is a policy about
   who may share with whom, which is the authorizer's question, and a
   workflow with notifications and an inbox, which is a product. Neither
   belongs in a storage core. The hosted platform that builds on Arca
   keeps its queue and calls `share.create` once it has decided.
2. **Email invites.** The `email` grantee kind, the invite mailer, and the
   lazy resolution that matched `claims.Email` on every request. Resolving
   an address to a subject is the issuer's job, and reading `email` for
   meaning is what invariant 5 forbids. A platform that invites by address
   resolves the address itself and creates a `subject` grant.
3. **Role grants.** The `role` grantee kind, which matched a name against
   the `roles` claim. Same reason.
4. **Team grants.** Already removed by drive's own migration
   `000017_drop_team_grants`, for this reason, before Arca existed.

## Not in this spec

The decision a grant takes part in, and the built-in owner policy
([[006-identity]]). The table and its indexes ([[004-metadata-store]]).
The status codes, pagination, and error bodies ([[013-api]]). What a read
of a shared path does once allowed ([[005-files]]). Usage accounting on a
shared write: bytes are charged to the space that owns them, never to the
grantee ([[010-events-and-reaper]]).

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | `Covering` returns only active, unexpired grants whose prefix covers the path, highest permission first, and `files/reports` does not cover `files/reports-archive` | `internal/shares` table test over the prefix, status, and expiry cases |
| 2 | Every share and link route asks the action its row names before it acts | [[017-conformance-suite]]'s row per action, driven against a recording authorizer |
| 3 | A `share.create` question carries the permission and the grantee, so a refusal of escalation is possible on the deciding side | the same, asserting the resource fields of the recorded question |
| 4 | A create of a `link` or `public` grant with `write` or `manage` is refused | `internal/shares` test |
| 5 | The three token routes serve with no bearer token, and answer not-found for a revoked, expired, or unknown token before any question is asked | e2e against a recording stub authorizer, asserting no question for an unresolvable token |
| 5b | A resolvable token asks `link.read` with the grant's `id`, `owner`, and `path` and an empty subject, and an authorizer that denies it stops every link | the same, driven twice against an allowing and a denying stub |
| 6 | A token route refuses a path outside the grant's prefix | the same |
| 7 | A revoke takes effect on the next request with no sweep in between | e2e: read, revoke, read again |
| 8 | `GET /v1/shares/with-me` answers grants where the caller is the grantee and reads no claim beyond the subject | e2e with two subjects from one issuer |
| 9 | An organization grantee takes the same code path as a person, with no group table and no org claim read | e2e where the authorizer names an organization subject as the grantee |
| 10 | A create and a revoke each append one event with the grantee kind in the detail | [[010-events-and-reaper]]'s tail test |
| 11 | No handler in `internal/shares` reads `org_id`, `roles`, or `email` | the `identity` gate's rule, plus a grep test in `internal/shares` |
