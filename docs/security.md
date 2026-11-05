# Security

What an installation of Arca guarantees the people who use it, what the
server can see, and what whoever runs it must keep safe. The
commitments, each with the test that holds it, what is not defended, and
how to report a vulnerability are in [`SECURITY.md`](../SECURITY.md).

## What Arca guarantees

Every request carries a token from an issuer you listed, and nothing is
read or written before your authorizer, or the built-in owner policy, has
decided. A decision that cannot be given is a refusal, and a refused
object answers exactly as a missing one, so a caller learns nothing about
what it may not see. A presigned URL names one object, one method and one
expiry. A public link reaches the object or subtree its token names and
nothing beside it, and what a link serves is a download and never a page
of your server's origin. [What is
protected](../SECURITY.md#what-is-protected) names the test behind each.

## What Arca sees

Arca keeps data in three places: your bucket, your Postgres database, and
the logs and telemetry the process writes. It has no store of its own. A
reader of one of them sees what the table says and nothing from the
others.

| What | Kept in | Who can read it |
|---|---|---|
| File contents | the bucket, as the client sent them, under a key made from the object's id and `ARCA_BUCKET_PREFIX`. Arca sends no encryption setting with a write | whoever holds the bucket or its credentials, and anyone with a presigned URL or a public link for the object until it expires or is revoked |
| Names and paths | Postgres: a file's path and owner, and the path in a version, a star, a share, an upload session and an event. The bucket key holds no path or name | whoever reads the database; callers see them through the API as the authorizer's decisions allow |
| Sizes, checksums and content types | Postgres, on the file and on each version. The content type is also stored with the object in the bucket | whoever reads the database or the bucket |
| Versions and the trash | the bytes of an earlier version, and of a trashed file, stay in the bucket. Postgres holds a row for each, with the time it was superseded or trashed | the same readers as file contents and paths |
| Stars | Postgres: the subject who starred, the owner and the path | whoever reads the database; a caller reads its own stars |
| Shares and grants | Postgres: the space granting, the path prefix, the grantee's subject, the permission, who created it, when it expires and whether it is revoked | whoever reads the database; callers see them as the authorizer's decisions allow |
| Links | Postgres: a link or public grant is a share row that carries its token as issued. The token is answered once, when the link is made, and is absent from every listing | whoever reads the database can use every live link. A public object also carries a public-read mark in the bucket |
| Events | Postgres: for each change to a space, its action, the path, the subject that did it and a few details such as a size or the kind of a share. Never file contents | whoever reads the database; a caller reads a space's events as the authorizer's `event.read` decision allows |
| Subjects | Postgres: a subject, the issuer and the token's `sub` joined by `\|`, appears as an owner, a creator, a grantee or an actor. Arca keeps no name, address or profile for it | whoever reads the database |
| What the authorizer is told | with `ARCA_AUTHORIZER_URL` set, each question carries the subject, every verified claim of the token, the action, the resource, and the request's id, client address and user agent | the authorizer you run. The question is sent over the connection you configured, so use `https` |
| Request logs | JSON on standard error: one line per request with the route pattern, the method, the status, the error code, the duration, the subject, the request id and the trace id. The route is the pattern a route is registered under, never the path a caller sent. No token, credential, presigned URL, file path or file name is a field of the line | whoever reads the process's standard error. With a collector set, the lines also go there |
| Traces | with a collector set, one span per request and one per bucket call. A request span records the path the request was sent, which names the space and the file, so a trace holds file paths. A link token in a path is replaced by `{token}` before the span leaves the process | whoever reads your collector |
| Metrics | `/metrics` on the internal listener: counts and timings by route pattern and status class, bucket call counts, reconciler findings, and usage totals across the installation. No path, file name or subject is a label, and usage is never per space | whoever can reach the internal listener |

The request line is built from those fixed fields, so a token, a
presigned URL or a path is kept out of it by what the line is made of.
Arca has no filter that rewrites a secret found in a log value, so a
change that logs a new field is a change to what is kept. Logs and traces
are covered in [Operating Arca](operations.md#traces-and-logs).

## How long it keeps things

A trashed file stays restorable for the trash retention, and a grant that
expired is kept for the same window before it is removed. The reconciler
that enforces both also removes the bytes nothing points at, aborts upload
sessions that expired, prunes stars whose file is gone and prunes old
events. [Housekeeping](configuration.md#housekeeping) states the retention
and the interval, and [Running the reconciler on its
own](operations.md#running-the-reconciler-on-its-own) states what each of
its passes removes and the age of the events it keeps.

Versions are kept until their file is deleted for good. A write, a
delete or a restore can be told by the authorizer to keep none, and a
delete made permanent never enters the trash; both are in
[What an answer may limit](authorizer.md#what-an-answer-may-limit), and
[Trash and stars](api.md#trash-and-stars) covers the trash itself. A
revoked grant stays in the database as a record that it existed.

## What it does not defend

Arca does not encrypt file contents. Whoever holds your bucket reads every
object, and whoever holds your database reads every path, grant, event and
live link token. A link token and a presigned URL are bearer credentials
until they expire or are revoked, and nothing recalls a signed URL. Arca
serves plain HTTP, follows whatever your authorizer and issuers decide,
and has no defense against a compromised host, bucket or database, or a
flood beyond its per-subject and per-address limits. [What is not
defended](../SECURITY.md#what-is-not-defended) lists each limit and who
can use it. Arca has had no external security audit.

## What an operator keeps safe

Arca has no signing key and no link secret of its own: a link token is a
random value stored in the database, and a presigned URL is signed with
the bucket credential.

| What | Why |
|---|---|
| The bucket credentials, `ARCA_BUCKET_ACCESS_KEY` and `ARCA_BUCKET_SECRET_KEY`, or the role the SDK's credential chain finds when both are unset | they read and write every object and sign every presigned URL. Keep them in a Secret, and give them access to the bucket and nothing beside it |
| The database URL, `ARCA_DB_URL`, and `ARCA_DB_POOL_URL` when set | they carry the database password, and the database holds everything in [What Arca sees](#what-arca-sees), including every live link token. The URL also carries the TLS mode of the connection, so set it |
| The authorizer's bearer, `ARCA_AUTHORIZER_TOKEN` | it is what your authorizer takes as Arca's own. Whoever holds it can put questions to your endpoint as Arca. Setting `ARCA_AUTHORIZER_URL` without it stops the server from starting |
| The authorizer itself, `ARCA_AUTHORIZER_URL` | it decides every action. One that allows everything allows everything. `arcad check` asks it a question every authorizer must deny and fails the installation on an allow, so run it after every change to the endpoint |
| The issuer list, `ARCA_OIDC_ISSUERS`, and the audience, `ARCA_OIDC_AUDIENCE` | an issuer that signs a token for the wrong subject lets its holder act as that person. List only issuers you run or trust to that degree. An `http` issuer is accepted on loopback only |
| `ARCA_OIDC_INSECURE_ISSUERS` | `true` admits an `http` issuer that is not on loopback, which lets the key set be fetched over a channel that can be rewritten. It is for a test stack only; leave it unset |
| Administrator subjects, `ARCA_ADMIN_SUBJECTS` | each subject listed, written `<issuer>\|<sub>`, is an administrator of every space. The list is read only when no authorizer endpoint is set. With neither a listed subject nor an endpoint, nobody is one |
| `ARCA_TRUSTED_PROXIES` | the ranges whose `X-Forwarded-For` Arca believes for the client address that its rate limit and the authorizer's `request.ip` use. List your ingress and nothing wider: from any other peer the header is ignored, so a caller cannot choose the address it is limited under |
| `ARCA_PUBLIC_CDN_URL` | when set, the owner's read of a public object redirects to that host without an expiry, and the host answers it under the type it was written with, so a page someone stored runs there. Serve it from a host that serves no pages, on a registrable domain apart from your installation's and from any sign-in |
| `ARCA_TEST_DRIFT` | it makes the build answer part of the API wrong so the conformance suite can prove it notices. Leave it empty in every deployment |
| TLS in front of the public listener | `arcad` serves plain HTTP, and every request carries a bearer token, a link token or a presigned URL. Terminate TLS at your ingress, and set `ARCA_PUBLIC_URL` to an `https` origin |
| The internal listener, `ARCA_INTERNAL_ADDR` | it serves the probes and `/metrics`, and is protected by nothing routing to it rather than by a token. Never route it, and do not let a `Service` or ingress name its port. The public listener answers `/metrics` with a 404 |
| The network policy | the base admits the two listeners and lets a replica reach DNS, HTTPS, Postgres and an OTLP collector and nothing else, which is what a compromised replica is confined to. See [Network policy](operations.md#network-policy) before you widen it |
