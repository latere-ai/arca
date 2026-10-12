# Architecture

How `arcad` is put together, for someone about to change it. The user's view
of the same system is the [API guide](../api.md); the reasoning behind each
decision is in the design records under [`specs/`](../../specs/README.md),
starting with the architecture record, `specs/001-architecture.md`.

## Two stores, one stateless process

```mermaid
flowchart LR
  client[client or sandbox] -->|/v1, bearer| arcad
  client -->|presigned GET and part PUT| bucket[(S3 bucket)]
  arcad -->|verify tokens| issuer[OIDC issuer]
  arcad -->|ask before every action| authz[authorization endpoint]
  arcad -->|bytes at id-derived keys| bucket
  arcad -->|rows, grants, events, usage| pg[(Postgres)]
```

`arcad` keeps nothing on local disk. What it holds in memory is a cache it
can rebuild: the rate-limit buckets, the authorizer's decision cache, and
the issuers' key sets.
Every replica serves every request. The bucket holds bytes and nothing
else; the database holds every fact about them: which path an object is at,
who wrote it, its versions, who may read it, what a space holds, and what
happened to it.

A space is one tree. Every path begins with `files/`, which `object.Plane`
names and `object.SplitPath` enforces, and there is no directory row: a
folder is a prefix, synthesized in a listing from the objects under it.

The database is the authority on whether an object exists. A key in the
bucket that no row names is garbage, and a row whose key is missing is an
alarm.

## The write and delete order

Two orders, chosen so that every failure between the two stores leaves
bytes nothing points at, which the reconciler can remove, and never a row
that points at nothing.

```mermaid
sequenceDiagram
  autonumber
  participant C as client
  participant A as arcad
  participant B as bucket
  participant P as Postgres
  C->>A: PUT /v1/files/{owner}/{path}
  A->>P: read the current row
  A->>A: ask the authorizer file.write
  A->>B: PUT <prefix><shard>/<new id>, If-None-Match: *
  A->>P: tx: lock row, check precondition, capture version, upsert row, charge usage
  alt the transaction refused
    A->>B: DELETE the fresh key
  end
  A-->>C: 201 or 200, ETag
  Note over A,P: a delete runs the other way: rows first, then keys
```

- **A write goes to the bucket first**, at a key derived from a freshly
  minted id (`object.NewID`, a UUIDv7). Nothing else can reference that key
  yet, so a failure after the bucket write leaves an orphan and nothing
  worse. The row is written in one transaction that repeats the
  precondition in SQL, so two writers of one path cannot both commit.
- **A delete goes to the database first.** Trashing a file sets
  `deleted_at` and leaves the bytes. A purge, a permanent delete, or a
  pruned version removes the row, then the key; a failure between the two
  leaves an orphan.
- **Keys never derive from paths.** `object.ID.Key(prefix)` is
  `<prefix><shard>/<id>`, where the shard is the id's last two characters,
  which are random and spread writes over 256 prefixes. A move is a row
  update. An overwrite writes a new key and keeps the old one for the
  version it becomes, so capturing a version copies no bytes.

The S3 client in `internal/blob` sends every put with `If-None-Match: *`.
A store that answers `NotImplemented` to it is remembered for the life of
the process, logged once, and written to without the header; keys are fresh
ids, so the guard is a second line and not the only one.

## The request path

```
request id -> observation (metrics, one log line) -> verifier -> per-subject rate limit -> route handler
```

The route table is a declaration. `internal/api/routes.go` holds the rows
the frame answers itself (events and the three public link routes), and
each owning package declares its own rows through `Table()`:
`internal/files`, `internal/uploads`, `internal/shares`, and
`internal/admin`. `cmd/arcad` concatenates them
and hands them to `api.New`, which builds the mux and the OpenAPI document
from the same list, so a route cannot be registered without being
described or described without being registered. `tools/apidoc` renders
the same list to `api/openapi.yaml`, and a test fails when the committed
file differs.

Every row names the one action it asks. The handler resolves the target,
asks `internal/auth`, and only then reads or writes. A route mounted under
the base path that no row registers still passes the verifier first, so an
unauthenticated caller gets 401 before it can learn that a path is unknown.
The three public link routes are registered outside the verifier, charge
the per-address rate limit instead, and ask `link.read` with an empty
subject.

`ARCA_BASE_PATH` is applied in one place, `api.under`, which swaps the
declared leading `/v1` for the configured base. The mux and the served
document both call it.

## Identity

`internal/auth` holds the verifier, the authorizer, and the owner policy.

- **The verifier** fetches each issuer's discovery document and key set at
  start and verifies every bearer (`latere.ai/x/pkg/authkit/jwt`). An
  issuer that does not answer at start is retried in the background and
  fails readiness until it does; the replica still starts.
- **The authorizer** is either the client of `latere.ai/x/pkg/authz`
  pointed at `ARCA_AUTHORIZER_URL`, or the owner policy. Both receive the
  same question. Before asking, `internal/auth` looks up the rung the
  caller holds through Arca's own grants table and puts it on the resource
  as `grant`, so an external endpoint can honor Arca's shares without
  reading its database. A question that names a `from`, a move or a copy,
  carries the lower of the rungs held on the two paths, so a grant on one
  end confers nothing.
- **The owner policy** (`policy.go`) allows the owner, the subjects in
  `ARCA_ADMIN_SUBJECTS`, a grantee whose rung reaches the action on the
  permission ladder, and a live link for `link.read`. It always denies the
  probe id, and it sends no limits.

Arca reads no claim for meaning. Claims are passed to the authorizer
verbatim; a plan, a team, or a role is the endpoint's to interpret.

A decision's `limits` object is decoded whole in one place,
`auth.ReadLimits`, which `files.LimitsOf` wraps for the handlers. A member
of the wrong type, or an `omit` entry that is not a path under `files/`,
fails the decode, and the handler answers `storage_unavailable` and writes
nothing, so no handler acts on part of an answer it could not read. Arca
stores none of the four members; each handler reads them off the answer to
its own question:

| Member | Where it is honored |
|---|---|
| `limits.quota_bytes` | inside the write's transaction, against `space_usage` |
| `limits.versions: false` | in the one write every put, upload completion, and copy commits through (`Write.NoVersion`), and in the restore of a version: the replaced content is not captured, and its bytes are dropped after the commit when nothing else references them |
| `limits.trash: false` | in the delete handlers, which take the permanent arm the request's `?permanent=1` takes |
| `limits.omit` | in the query of every enumeration, as one predicate (`notOmitted` in `internal/store`) matched on whole segments, so a page is never short for a row it left out and a count counts only what the caller may enumerate |

A `filter` on a list action narrows that list's query.

## The packages

| Package | What it holds |
|---|---|
| `cmd/arcad` | wiring only: configuration, the two listeners, the subcommands, and the adapters that bind one package's seam to another's (the ledger, the restorer, the two passes other packages own) |
| `internal/config` | every `ARCA_*` variable, read once, with one error naming every problem |
| `internal/api` | the router, the error envelope and code table, the list envelope, ETags and preconditions, the request id, the two rate limits, the base path, and the served OpenAPI document |
| `internal/apidocs` | the OpenAPI document built as Go values, so the server needs no YAML library |
| `internal/auth` | the verifier, the authorizer client, the owner policy, the grant lookup, and the reading of an answer's limits |
| `internal/blob` | the bucket: the S3 client, an in-memory store for tests, server-side copy, and presigning (five minutes for a read, 24 hours for an upload part) |
| `internal/store` | Postgres: one query set per table, the transaction helper, and the migrations |
| `internal/files` | the tree of a space: put, get, head, list, move, copy, delete, the folder delete and the folder copy, versions, trash, stars, and materialize |
| `internal/uploads` | multipart sessions: open, complete, abort, and the expiry sweep |
| `internal/shares` | grants and links, token minting, the public-object stamp, and the expired-grant sweep |
| `internal/events` | the event log, the usage ledger, and `GET /v1/events` |
| `internal/reaper` | the reconciler and its passes |
| `internal/admin` | the overview across spaces and the cross-space restore |
| `internal/check` | the `arcad check` requirements |
| `internal/metrics` | every metric name and the bucket decorator that counts and times each call |
| `internal/version` | the build identity, set by `-ldflags` |
| `object` | exported: object ids, keys, the plane a path is rooted in, and checksum kinds |
| `authorizer` | exported: the action vocabulary and the resource shapes an authorization endpoint is written against |
| `test/conformance` | exported: the black-box suite that drives any installation over HTTP |

The two exported root packages promise additive change within a module
major: an action string, a resource field, or a key shape never changes
meaning. `internal/` promises nothing to anyone outside the module.

`tools/` holds commands that are not part of the server: `apidoc` renders
`api/openapi.yaml`, `rules` renders the alert rules, `smoke` holds the
release pipeline's smoke script, and `migrate-drive` and `move-objects`
are the one-time migration tools described in
[Migrating from Drive](drive-migration.md).

## The schema

Six forward-only migrations under `internal/store/migrations`, applied by
`arcad migrate` through `latere.ai/x/pkg/pgxmigrate`. A server refuses to
start against a database behind its own migrations, and readiness fails
while the schema is behind. The sixth, `0006_one_tree`, moves rows and
drops two tables, and [operations](../operations.md#the-one-tree-migration)
says what that means for an upgrade.

| Table | What a row is |
|---|---|
| `subjects` | a subject Arca has seen, with a display name and when it was last seen |
| `files` | the live content at one `(owner, path)`: its object id, size, checksum and kind, content type, public flag, and `deleted_at` when trashed |
| `file_versions` | a superseded content of a path, numbered per path, pointing at the object id it had |
| `stars` | one subject's star on one `(owner, path)` |
| `upload_sessions` | an open multipart upload: the object id it will land at, the store's upload id, the declared size, and its expiry |
| `shares` | a grant: owner, path prefix, grantee kind (`subject`, `link`, or `public`), grantee or token, permission, status, and expiry. Constraints keep a token grant at `read` |
| `space_usage` | the bytes one space holds |
| `events` | the log: owner, path, action, actor, detail, and time, with a serial id that is the cursor |

## Uploads

`POST /v1/uploads` mints the object id first, opens a multipart upload in
the bucket at that id's key, records the session, and presigns one `PUT`
URL per 16 MiB part. The client sends the parts straight to the bucket.
`complete` asks the bucket to assemble the parts, then commits the row
through the same `files.Commit` a `PUT` uses, so preconditions, version
capture, and the usage charge behave identically. The object's checksum is
then the store's composite ETag, recorded with `checksum_kind: etag`.

An incomplete multipart upload's parts do not appear in a bucket listing,
so the session row is the only pointer to them. The reconciler treats the
session table as a set of referenced keys, and a session past its 24 hour
expiry is aborted.

## Folders

A folder is every row whose path begins with the folder's path and a slash
(`under` in `internal/store`), never the row at exactly that path. Two
handlers in `internal/files` take one whole, and both ask the authorizer
once, with `recursive` on the question.

**The folder delete** is one statement at any size. The soft form sets
`deleted_at` on every live row under the folder (`TrashSubtree`). The
permanent form, taken on `?permanent=1` or on an answer that carries
`limits.trash: false`, runs one transaction: it deletes the rows, live and
trashed, with their versions (`DropSubtree`), deletes the grants and links
whose prefix is the folder or under it, and gives the bytes back to
`space_usage`. The grants go for the reason a move carries an exact-path
grant with it: the next object written there must not inherit one. The
keys go after the commit, a thousand to a bucket call, each batch through
the reference check first. A request that ends before the last batch
leaves the rest for the reconciler's first pass.

**The copy** is done by the bucket. No row can share an object with
another: publicity is a setting on the bucket key, so two paths on one key
could not be public and private apart, and every delete of bytes would
depend on a reference check that never sees a second row for one id. So
each object is copied inside the store to the key of a freshly minted id.

```mermaid
sequenceDiagram
  autonumber
  participant C as client
  participant A as arcad
  participant P as Postgres
  participant B as bucket
  C->>A: POST /v1/files/{owner}/{folder} {copy_to, recursive}
  A->>P: count the live rows under the folder, sum their sizes
  A->>A: ask the authorizer file.write once: path, from, copy, recursive, size
  A->>P: read the rows the answer leaves in, with the path each lands on
  A->>B: Copy each object to a new id, eight in flight
  A->>P: tx: confirm the sources are unchanged, write every row create-only, charge usage
  alt the transaction refused
    A->>B: DELETE the keys this call wrote
  end
  A-->>C: 200 {path, files, bytes}
```

The order is the write order above, bucket first and rows second. Every
destination row goes through the same `commit` a `PUT` uses, with the
create-only precondition, in one transaction, so a caller sees the whole
copy or none of it: a taken destination is `path_taken`, a charge past the
answer's limit is `quota_exceeded`, and a source row that changed since it
was copied is `precondition_failed`. A copy is bounded at
`files.MaxCopyObjects`, 10,000, because the request stays open while the
bucket works, and runs `files.CopyConcurrency` copies at a time.

Both handlers enumerate, so both honor `limits.omit`: an omitted subtree is
neither trashed, removed, nor copied, and the counts in the answer and in
the event leave it out.

## Events and usage

`internal/events` holds two things written from other packages through
seams `cmd/arcad` binds:

- **The log.** One row per change, with an action from a closed
  vocabulary of nine; an append with an unknown action is refused. Grant
  mutations append inside their own transaction, so the change and its
  event commit together. File mutations append after their commit, best
  effort, and a failed append is logged rather than returned. A folder
  delete and a copy append one event each, with the counts in its detail.
  `GET /v1/events` reads the log by keyset on the serial id. Rows older
  than 30 days are pruned.
- **The usage ledger.** `space_usage` is charged inside each write's
  transaction. A charge that would cross the authorizer's
  `limits.quota_bytes` is refused and rolls the write back. The reconciler
  recomputes the ledger from the rows and corrects drift.

## The reconciler

`internal/reaper` runs eight passes in order. Every pass is idempotent and
conditional on the state it read, so several replicas may run it at once
without a lease. A pass that fails does not stop the run; the run reports
failure at the end.

| Pass | What it does |
|---|---|
| 1 | removes keys under the prefix that no row, version, or open session references, after a 24 hour grace |
| 2 | reports rows whose key the bucket does not hold (`missing_bytes`); deletes nothing |
| 4 | aborts upload sessions past their expiry |
| 5 | purges trash past `ARCA_TRASH_RETENTION`, rows then keys, with the ledger delta in the same transaction |
| 7 | removes grants that expired longer ago than the same window |
| 8 | prunes stars whose file no longer exists at all |
| 9 | prunes events older than 30 days |
| 10 | recomputes `space_usage` and corrects rows that disagree |

The numbers are the design record's. Passes 3 and 6 are retired, and
their numbers are not reused.

`arcad serve` runs the passes every `ARCA_REAP_INTERVAL`, and `arcad reap`
runs the same eight as a process of its own. `-dry-run` reports every
finding and changes nothing. Each finding is counted on
`arca_reaper_findings_total{kind, action}`.

## Observability

`internal/metrics` declares every metric name, and every one is on
`/metrics` from the first scrape. No label carries a subject, an owner, or a
path; those go on spans. Traces, log records and the request metrics leave
through `latere.ai/x/pkg/otel` over OTLP when an endpoint is configured. Each
listener's handler is wrapped in that package's `Handler`, outside the
verifier, so every request but the probes and the scrape is one SERVER span
and one measurement of `http.server.request.duration`. Both are named by the
row of the route table the request matches, read from the router before the
request is served (`api.API.Route`), and a bucket call is a child of the
request's span because the handler passes the request's context down. The
span's `url.path` is the path as sent, with a link's token replaced by
`{token}` (`api.API.SpanPath`). Each request writes one JSON log line with
the route pattern (never the raw path), the status, the error code, the
duration, the subject, the request id, and the trace id.
