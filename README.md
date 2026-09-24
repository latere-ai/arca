# Arca

**Durable storage for people, agents, and sandboxes.** Files with versions
and trash, shares and public links, workspaces a sandbox attaches to and
syncs back, usage per space, and an event log, over any S3 compatible
bucket and a Postgres database. Identity comes from any OpenID Connect
issuer. Permission comes from an endpoint you write, or from a built-in
owner policy when you write none.

Latere runs Arca in production as the storage behind its platform. This
repository is that service, and anyone with a bucket, a Postgres database,
and an issuer can run their own.

[![CI](https://github.com/latere-ai/arca/actions/workflows/verify.yml/badge.svg)](https://github.com/latere-ai/arca/actions/workflows/verify.yml)
[![Release](https://img.shields.io/github/v/release/latere-ai/arca)](https://github.com/latere-ai/arca/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/latere-ai/arca)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

## The problem

A platform that runs agents and sandboxes needs a place for what is
neither a commit nor a sandbox's local disk: the file a person uploads,
the artifact an agent produces, the working tree a sandbox needs back
tomorrow. A git host holds commits. A sandbox's volume dies with the
sandbox. An object store holds bytes but knows nothing about who owns
them, who may read them, how much a space holds, or what happened to
them last week.

Arca is that place. It gives every principal a space, every file a version
history and a trash, every share a permission, every space a usage count,
and every change an event a consumer can tail.

## How it works

- **The bucket holds bytes, the database holds meaning.** A write goes to
  the bucket first and to the database second; a delete goes to the
  database first and to the bucket second. A failure between the two
  leaves bytes nothing points at, never a record of bytes that are gone,
  and a reconciler removes the leftovers.
- **Keys come from ids, not paths.** Every write of content gets a fresh
  id and its own key, so a move or a rename is a database update, an
  overwrite keeps the old version without copying it, and one bucket can
  hold several installations under different prefixes.
- **Bytes stay off the server.** A large read is a redirect to a
  short-lived presigned URL, and a large upload goes to the bucket in
  parts the client sends directly. The server handles metadata,
  authorization, and small objects.
- **A workspace has one writer at a time.** A sandbox attaches, takes a
  writer lease, downloads the tree, works on its own disk, and syncs the
  result back. The lease expires on its own, so a sandbox that crashes
  does not hold the workspace.
- **Any replica serves any request.** Nothing is kept on local disk and
  there is no leader. Scale the Deployment.

## Identity

Arca verifies and asks. Every request carries a token from an issuer you
list; Arca checks it against the issuer's key set and reads nothing else
from the issuer. Whether the caller may act is a question Arca sends,
with the verified claims and the action, to an authorization endpoint you
write. Without one, the owner policy applies: a subject acts on its own
space, the administrators you list act on every space, and anyone else
gets what a grant or a link gives them.

The contract is shared with [Origo](https://github.com/latere-ai/origo),
[Cella](https://github.com/latere-ai/cella), and
[Lux](https://github.com/latere-ai/lux), so one endpoint can answer for
all four.

## Try it in a few minutes

You need Go 1.27 or newer, `git`, `curl`, and Docker or Podman with
Compose.

```sh
git clone https://github.com/latere-ai/arca.git
cd arca
make run
```

That builds `arcad`, starts Postgres and MinIO with a bucket, starts a
stub issuer and a stub authorizer, applies the database migrations, and
runs the server in the foreground. Once it is ready it runs `arcad check`
against itself, one line per requirement, and then prints a token and two
requests to paste into another terminal:

```
export ARCA_URL=http://localhost:<port> ARCA_TOKEN=<token>
curl -sS -X PUT -H "Authorization: Bearer $ARCA_TOKEN" ... "$ARCA_URL/v1/files/<you>/files/hello.txt"
curl -sS -H "Authorization: Bearer $ARCA_TOKEN" "$ARCA_URL/v1/files/<you>/files/hello.txt"
```

The first writes an object to your own space and the second reads it
back. Ports derive from the checkout's directory name, so two clones run
side by side, and everything listens on loopback. Stop the server with
Ctrl-C, then `make run-down` stops the stubs and `make clean` removes the
stack and its data.

## Install it for real

[`docs/install.md`](docs/install.md) goes from a Kubernetes cluster to a
serving installation. You supply four things: a bucket at any S3
compatible endpoint, a Postgres database, an OIDC issuer, and a hostname.
An authorization endpoint is optional.

## What you get

- **Files.** Put, get, head, list, move, and delete, with a version kept
  on every overwrite, a trash that holds deleted files for 30 days by
  default, stars, and conditional requests on the object's checksum.
- **Uploads in parts.** An object up to 5 GiB by default goes to the
  bucket in parts over presigned URLs, and the server assembles it.
- **Shares and links.** Grant a subject `read`, `write`, or `manage` on a
  subtree of a space, or mint a link whose token lets anyone holding it
  read.
- **Workspaces.** A named subtree a sandbox attaches to, read-only or
  with the writer lease, materializes as presigned URLs, and syncs back
  by declaring what the tree holds.
- **An event log.** Every change to a space is one event, read by cursor,
  so a consumer tails the log rather than receiving webhooks.
- **Administration.** Usage per space across the installation, and a
  restore of any space's deleted file or workspace.
- **An OpenAPI document** generated from the route table and served by
  every installation at `/openapi.json`.
- **A conformance suite** you run against your own installation, or
  against anything that claims to serve the same API.
- **Packages to build on.** `latere.ai/x/arca/object` (ids, keys, planes)
  and `latere.ai/x/arca/authorizer` (the action vocabulary an
  authorization endpoint is written against).

## Documentation

| | |
|---|---|
| [Install](docs/install.md) | from a cluster, a bucket, and a database to a serving installation |
| [Configuration](docs/configuration.md) | every environment variable with its default |
| [API](docs/api.md) | the model, every route, errors, and the authorization endpoint you write |
| [Operations](docs/operations.md) | checking an installation, upgrades and rollbacks, verifying a release, monitoring |

[`docs/README.md`](docs/README.md) is the index.
[`docs/internals/`](docs/internals/README.md) explains how Arca is built,
for people changing it, and the design records behind each decision are
in [`specs/`](specs/README.md).

## Contributing

Issues and pull requests are welcome. [`CONTRIBUTING.md`](CONTRIBUTING.md)
covers the quality gate, the test tiers, and how a change is reviewed.
[`SECURITY.md`](SECURITY.md) is how to report a vulnerability; please do
not open an issue for one.

## License

MIT. See [`LICENSE`](LICENSE).
