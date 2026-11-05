# Running it locally

`arcad` on your own machine, from a clone: Postgres and MinIO in
containers, a stand-in issuer and a stand-in authorizer, and requests
that show the server storing and returning files. One command does the
setup, and one more terminal sends the requests.

Everything here is for trying Arca. The stand-ins accept any request
for the one made-up person you get a token for, and the database and
the bucket keep their data in volumes that `make clean` deletes.
[Install](install.md) is the page for a real installation.

## What you need

- Go 1.27 or newer, to build `arcad` and the stand-ins.
- `git` and `curl`.
- Docker or Podman, with its `compose` command, for Postgres and MinIO.

`make` uses `docker` when it finds one on your path and `podman`
otherwise. To pick one yourself, set `DEV_ENGINE`:

```sh
make run DEV_ENGINE=podman
```

With Podman, the Compose commands and the tests also need to find
Podman's socket. If `make run` cannot reach the engine, point
`DOCKER_HOST` at it first:

```sh
export DOCKER_HOST="unix://$(podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}')"
```

## Start it

In one terminal, from the root of a clone:

```sh
git clone https://github.com/latere-ai/arca.git
cd arca
make run
```

`make run` stays in the foreground and prints the server's log. It
does these things, in this order:

1. **Builds** `out/arcad`, the server, and `out/arca-stubs`, the
   stand-ins.
2. **Starts the stack** from `compose.yaml`: Postgres 18, MinIO, and a
   one-shot container that creates the bucket `arca-test` and exits. It
   waits until MinIO answers, the bucket exists, and Postgres accepts
   connections:

   ```
   Postgres is at postgres://arca:arca@127.0.0.1:22224/arca?sslmode=disable
   MinIO is at http://127.0.0.1:22222 with the bucket arca-test
   ```

3. **Starts the stand-ins** in the background, once per checkout:

   ```
   the stubs are ready: issuer http://localhost:22225, authorizer http://127.0.0.1:22226
   ```

   The issuer signs tokens for anyone who asks at its `/mint` address
   and serves the keys that verify them. The authorizer allows every
   action except on one reserved resource that every authorizer must
   refuse, which is how `arcad` can tell that it really asks someone.
   [Writing an authorizer](authorizer.md) describes the real one, and
   the [design of the stand-ins](../specs/014-test-stubs-and-tiers.md)
   lists their options.

4. **Migrates** the database. It prints the command it runs, with every
   setting `arcad` reads, and then:

   ```
   arcad: the database holds every migration this binary carries
   ```

5. **Serves.** The server answers on the public address, and on a second
   address that carries the probes:

   ```
   arcad: the reconciler runs every 5m0s
   arcad: v0.12.1-10-ge8fbd3e listening public=127.0.0.1:22220 internal=127.0.0.1:22221 base=/v1 audience=arca deciding="authorizer"
   ```

   The version is that of your checkout, so yours differs.

6. **Checks itself.** Once the server is ready, `arcad check` asks the
   running installation five questions and prints one line for each:

   ```
   ok    bucket      arca-test at http://127.0.0.1:22222, prefix arca/: wrote, read, deleted
   ok    database    PostgreSQL 18.6, schema at 0006_one_tree, clean
   ok    issuer      http://localhost:22225: discovery ok, 1 keys, RS256
   ok    authorizer  http://127.0.0.1:22226: denied the probe resource
   ok    public-url  http://localhost:22220, serving under /v1: answers the version endpoint
   arcad: 5 checks passed
   ```

   | Check | What `ok` means |
   |---|---|
   | `bucket` | Arca wrote an object to the bucket, read it back, and deleted it |
   | `database` | Postgres answers and holds the newest schema, with no half-applied change |
   | `issuer` | the issuer's discovery document and key set can be read, and the keys are usable |
   | `authorizer` | the authorizer answers, and refused the one resource every authorizer must refuse |
   | `public-url` | the public address is reachable and serves the API under `/v1` |

   A failed check does not stop the server. The line says what is wrong,
   and the server stays up so you can fix it.

7. **Prints a token** for the made-up person `dev`, and two requests to
   try:

   ```
   export ARCA_URL=http://localhost:22220 ARCA_TOKEN=<token>
   curl -sS -X PUT -H "Authorization: Bearer $ARCA_TOKEN" -H "Content-Type: text/plain" --data-binary "hello, arca" "$ARCA_URL/v1/files/http%3A%2F%2Flocalhost%3A22225%7Cdev/files/hello.txt"
   curl -sS -H "Authorization: Bearer $ARCA_TOKEN" "$ARCA_URL/v1/files/http%3A%2F%2Flocalhost%3A22225%7Cdev/files/hello.txt"
   ```

   The token is good for one hour. When it expires, stop the server and
   run `make run` again for a new one: the data stays.

**The ports.** Every port counts up from a base that `make` computes from
the name of the directory you cloned into. For a checkout named `arca`
it is 22220, and the ports above are that base plus 0 for the API, 1 for
the probes, 2 for MinIO, 3 for its console, 4 for Postgres, 5 for the
issuer, and 6 for the authorizer. A directory with another name gets
other ports, which lets two clones run side by side, so read your own
from the printed lines. Everything listens on `127.0.0.1` only. To pick
the base yourself, set `DEV_PORT_BASE`.

**The owner in the path.** Every person has one space, and a path names
the person who owns it. Arca writes a person as the issuer and the
token's `sub`, joined by `|`, so the person here is
`http://localhost:22225|dev`. A path carries that as one segment, so the
characters `:`, `/` and `|` are percent-encoded:
`http%3A%2F%2Flocalhost%3A22225%7Cdev`. The token's own space can also be
written `me`, which the requests below use from here on.

## Send requests

Open a second terminal. Paste the `export` line from the first one, then
the two requests under it.

The first writes `files/hello.txt` and answers with the object:

```sh
curl -sS -X PUT -H "Authorization: Bearer $ARCA_TOKEN" -H "Content-Type: text/plain" --data-binary "hello, arca" "$ARCA_URL/v1/files/http%3A%2F%2Flocalhost%3A22225%7Cdev/files/hello.txt"
```

```json
{"path":"files/hello.txt","size":11,"checksum":"b187b04face9c9b61e42b714107d85f419c28281ac53bf8e10b4a607d427a1b7","checksum_kind":"sha256","content_type":"text/plain","is_public":false,"modified":"2026-11-05T19:05:35Z"}
```

The second reads it back:

```sh
curl -sS -H "Authorization: Bearer $ARCA_TOKEN" "$ARCA_URL/v1/files/http%3A%2F%2Flocalhost%3A22225%7Cdev/files/hello.txt"
```

```
hello, arca
```

The rest keep the token in a header variable and use `me` for the owner:

```sh
A="Authorization: Bearer $ARCA_TOKEN"
```

### List a folder

A folder is every file whose path starts with it, and Arca stores no
folder of its own. `?list=1` lists a path's contents, with `prefixes`
for the folders below it and `space` for what the whole space holds:

```sh
curl -sS -H "$A" "$ARCA_URL/v1/files/me/files?list=1"
```

```json
{"entries":[{"path":"files/hello.txt","size":11,"checksum":"b187b04face9c9b61e42b714107d85f419c28281ac53bf8e10b4a607d427a1b7","checksum_kind":"sha256","content_type":"text/plain","is_public":false,"modified":"2026-11-05T19:05:35Z"}],"prefixes":[],"space":{"bytes":11,"files":1}}
```

### Overwrite it and read the old version

Writing to a path that holds a file replaces it and keeps what was
there as a version:

```sh
curl -sS -X PUT -H "$A" -H "Content-Type: text/plain" --data-binary "hello again, arca" "$ARCA_URL/v1/files/me/files/hello.txt"
```

```json
{"path":"files/hello.txt","size":17,"checksum":"43fc0a2f422b27b30440c8ed00a078372cdf00a854709b6fc10be52e3246a4d3","checksum_kind":"sha256","content_type":"text/plain","is_public":false,"modified":"2026-11-05T19:05:35Z"}
```

```sh
curl -sS -H "$A" "$ARCA_URL/v1/files/me/files/hello.txt?versions=1"
curl -sS -H "$A" "$ARCA_URL/v1/files/me/files/hello.txt?version=1"
```

```json
{"entries":[{"version_no":1,"size":11,"checksum":"b187b04face9c9b61e42b714107d85f419c28281ac53bf8e10b4a607d427a1b7","checksum_kind":"sha256","content_type":"text/plain","created_by":"http://localhost:22225|dev","superseded_at":"2026-11-05T19:05:35Z"}]}
```

```
hello, arca
```

A plain read still answers the new content, `hello again, arca`.

### Delete it and find it in the trash

A delete moves the file to the trash, where it stays restorable for
the [trash retention](configuration.md#housekeeping). The delete
answers `204` with no body, and the next read says nothing is there:

```sh
curl -sS -X DELETE -H "$A" "$ARCA_URL/v1/files/me/files/hello.txt"
curl -sS -H "$A" "$ARCA_URL/v1/files/me/files/hello.txt"
```

```json
{"error":{"code":"not_found","message":"There is no such object.","details":{"detail":"there is no object at \"files/hello.txt\"","request_id":"req_06GTT6HA58WATB9S74GQJMAMMW"}}}
```

The trash lists it, with the day it is purged for good, and one request
brings it back:

```sh
curl -sS -H "$A" "$ARCA_URL/v1/trash"
curl -sS -X POST -H "$A" -H "Content-Type: application/json" -d '{"owner":"me","path":"files/hello.txt"}' "$ARCA_URL/v1/trash/restore"
```

```json
{"entries":[{"path":"files/hello.txt","size":17,"created_by":"http://localhost:22225|dev","deleted_at":"2026-11-05T19:05:35Z","purges_at":"2026-12-05T19:05:35Z"}]}
```

```json
{"path":"files/hello.txt","size":17,"checksum":"43fc0a2f422b27b30440c8ed00a078372cdf00a854709b6fc10be52e3246a4d3","checksum_kind":"sha256","content_type":"text/plain","is_public":false,"modified":"2026-11-05T19:05:35Z"}
```

### Share it with a link

A link gives whoever holds its token read access to one file or folder,
with no sign-in. The token is shown once, in the answer that creates the
link:

```sh
curl -sS -X POST -H "$A" -H "Content-Type: application/json" -d '{"owner":"me","path_prefix":"files/hello.txt"}' "$ARCA_URL/v1/shares/links"
```

```json
{"id":"e59f0920-6823-4222-a1b3-89c836fb85cd","owner":"http://localhost:22225|dev","path_prefix":"files/hello.txt","grantee_kind":"link","permission":"read","status":"active","token":"5iExTDAhdfc6dpvg7mr51IZCppsgYPvDttXm418UVnc","created_by":"http://localhost:22225|dev","created_at":"2026-11-05T19:07:52Z","url":"/v1/shares/links/5iExTDAhdfc6dpvg7mr51IZCppsgYPvDttXm418UVnc"}
```

Your token and id differ. Put them in variables, then read the file
without any `Authorization` header. The path after `/files/` is the
file's whole path:

```sh
TOKEN=5iExTDAhdfc6dpvg7mr51IZCppsgYPvDttXm418UVnc
LINK=e59f0920-6823-4222-a1b3-89c836fb85cd
curl -sS -i "$ARCA_URL/v1/shares/links/$TOKEN/files/files/hello.txt"
```

```
HTTP/1.1 200 OK
Content-Disposition: attachment; filename="hello.txt"; filename*=UTF-8''hello.txt
Content-Length: 17
Content-Security-Policy: sandbox
Content-Type: text/plain
Etag: "43fc0a2f422b27b30440c8ed00a078372cdf00a854709b6fc10be52e3246a4d3"
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
X-Request-Id: req_06GTT721KAS7EGQVP9D0YMBP48
Date: Thu, 05 Nov 2026 19:07:52 GMT

hello again, arca
```

A link read is always a download, which is what `Content-Disposition`
says, so a browser that opens the address saves the file rather than
showing it.

Revoking the link ends the access at once:

```sh
curl -sS -i -X DELETE -H "$A" "$ARCA_URL/v1/shares/links/$LINK"
curl -sS -i "$ARCA_URL/v1/shares/links/$TOKEN/files/files/hello.txt"
```

The first answers `204 No Content` and the second `404 Not Found`.

## Stop it

In the first terminal, press Ctrl-C. That stops `arcad` and normally the
stand-ins with it. The containers keep running, so the next `make run`
starts quickly and finds your data.

- `make run-down` stops the stand-ins, if Ctrl-C left them running.
- `make down` stops the containers and keeps their volumes, so the next
  `make run` finds the same files.
- `make clean` stops everything and deletes the containers, their volumes
  (the database and the bucket), and the `out/` directory. Run it to
  start over from nothing.

## What next

- [Install](install.md): a real installation, with your own bucket,
  database and issuer.
- [Configuration](configuration.md): every setting `arcad` reads, and
  the ones `make run` sets.
- [The API](api.md): every route, including uploads, shares, trash,
  events and administration.
- [Writing an authorizer](authorizer.md): deciding who may do what.
- [Testing](internals/testing.md): the tiers that run against this same
  stack.
