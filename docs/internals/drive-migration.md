# Migrating from Drive

The one-time procedure that moved Latere's predecessor storage service,
Drive, onto Arca. It ran in September 2026 and is kept as the record of
how `tools/migrate-drive` and `tools/move-objects` are used; the design
and its outcome are in [`specs/019-migration-from-drive.md`](../../specs/019-migration-from-drive.md).

Arca replaces a service called Drive. If you run that service, two commands
move it during the cutover, in this order: `migrate-drive` copies its metadata
into an Arca database, and `move-objects` copies its objects to the keys the
copied rows name. The first reads the old database and writes the new one; the
second copies inside your bucket. Neither changes anything in the old
database, and neither deletes an object during the cutover. Deleting the old
keys is a later step, run with a flag of its own once the new service holds.

Read this whole section before you start. Both commands have to succeed before
you switch a route.

## What to prepare

- **A fresh Arca database**, with the migrations applied and no rows in it.
  Run `arcad migrate` against it. The copy refuses a database that already
  holds rows, so a second attempt begins by dropping this database and
  migrating it again.
- **The issuer URL** your identity provider signs tokens with, for example
  `https://issuer.example`. Every personal space becomes the subject
  `<issuer>|<id>`, so this value has to be the one your installation verifies
  against. It is what `ARCA_OIDC_ISSUERS` names.
- **A mapping of organizations to subjects**, exported from your identity
  provider. Drive addressed an organization by its own id; Arca addresses it
  by the subject the provider assigns it, and nothing but the provider knows
  which is which. JSON or CSV, read by what the file begins with and not by
  its name:

  ```json
  {
    "33333333-3333-4333-8333-cccccccccccc": "https://issuer.example|org-acme"
  }
  ```

  ```csv
  organization,subject
  33333333-3333-4333-8333-cccccccccccc,https://issuer.example|org-acme
  ```

  The header row is optional. An organization in the old database that the
  file does not name stops the run before it writes anything, and names the
  id you have to add.

  **Or no file at all.** If your identity provider gives an organization the
  subject `<issuer>|<its id>` rather than a subject of its own, that is a rule
  and not a table: pass `-org-issuer https://orgs.example` instead of
  `-org-subjects`, and every `o-<id>` becomes `https://orgs.example|<id>`. The
  issuer is often not the one your people carry, which is why it is its own
  flag. Give one or the other; both is a command line to correct.
- **Drive in read-only mode.** The copy is a snapshot. A write that lands
  after a table has been read is a row the new database does not have and
  nothing will tell you about. Turn the read-only flag on and confirm writes
  are refused before you start, and leave it on until you have switched your
  routes.

## Copying the rows

It runs from a checkout of this repository, with the Go toolchain and reach to
both databases. It is not in the server image: it runs once, from wherever you
can reach the two databases, and never from inside the service.

```sh
go run ./tools/migrate-drive \
  -source 'postgres://user:password@old-db/drive?sslmode=require' \
  -target 'postgres://user:password@new-db/arca?sslmode=require' \
  -issuer https://issuer.example \
  -org-subjects orgs.json \
  -manifest manifest.tsv
```

Add `-dry-run` to read, rewrite and report without writing. A dry run prints
the report a real run prints, so run it first: it tells you which rows will be
dropped and whether the mapping is complete, and it leaves the target alone.

`-manifest` writes the file the second command reads. Keep it: it is the only
record of which old key became which object id, and without it nothing can
move your objects. It is tab separated, one line per distinct old key, and the
last line says the copy verified:

```
#arca-manifest	1	drive/
drive/u-1/files/notes.md	0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f	21	3b1f…	false
#complete	1
```

A dry run writes no manifest; it writes no rows, so there are no ids for it to
be the record of.

`-prefix` defaults to `drive/` and is the bucket prefix the old installation
wrote its keys under. The run checks it twice: every key in the source has to
begin with it, and it has to match `ARCA_BUCKET_PREFIX` where that variable is
set, so a copy cannot be made under one prefix for a server deployed with
another.

The run exits 0 when every table verified, 1 when anything went wrong or was
refused, and 2 on a bad flag.

## What the copy's report means

```
table                  copied  dropped  verified
subjects               2       0        counts hold
files                  4       0        counts hold, and 4 checksums were sampled
shares                 5       4        counts hold
space_usage            2       0        recomputed over 2 spaces
```

- **copied** is the rows now in the new database.
- **dropped** is the rows that did not come across, listed under `rows
  dropped` with the reason for each. Four reasons, all of them deliberate:
  grants to a role, to a team, or to an email address, which Arca does not
  have; share requests, which are the grants that were pending or denied and
  are your platform's to hold now; and link or public grants that allowed
  writing, which Arca's links do not. Read these counts. They are access
  somebody had yesterday and will not have tomorrow.
- **verified** is what was checked. `counts hold` means the old database held
  as many rows as the run accounted for, copied plus dropped, and the new one
  holds as many as it wrote. For files a sample of rows is read back and
  compared on checksum and size. For `space_usage`, which is recomputed
  rather than copied, the ledger is compared against the sum the run made
  while reading.
- **noted** lists what changed inside a row without dropping it: tokens
  minted for a link or public grant that had none, invite tokens cleared
  where the grantee became a subject, repositories that became workspaces,
  `agents_folded` for each row that left the retired agent zone, actions in
  the log that Arca's vocabulary does not have, and upload sessions still
  open.
- **manifest**, in the header block, is where the file was written, how many
  old keys it lists, and whether it is complete. Only a run that verified
  completes it, and the second command refuses one that is not complete.

Any table that does not verify makes the run exit 1 and names what it found.
Do not switch your routes on a run that exited 1, and do not move the objects
on one either.

## What changes on the way

- Owners become subjects. `u-<id>` becomes `<issuer>|<id>`, and `o-<id>`
  becomes the subject your mapping gives it.
- Paths move plane. Arca has two, `files/` and `workspaces/`. `memory/`
  becomes `files/memory/`, `agents/` becomes `files/agents/`, and
  `repos/<name>/` becomes `workspaces/<name>/`. The workspace keeps its name;
  it stops being a separate kind of thing.
- The `agents/` rows land in the same space they were in, counted in the
  report as `agents_folded`. Arca has no rule that keeps a machine out of
  where a person curates, so those files are files: read the count, because
  the person who owns the space can now see them in their own plane. The
  objects do not move for the fold; a bucket key derives from an object id and
  carries no path.
- A path in any other plane stops the run. If your installation holds one,
  decide where those rows belong before you migrate.
- `quotas`, `webhooks`, `agent_visibility` and `admin_audit` are not copied.
  Arca counts what a space holds and stores no limit; the event log is the
  integration point; audit is the event log.
- The event log keeps every id, so a consumer holding a cursor keeps its
  place.

## Moving the objects

**The copy moves rows and not objects, and setting `ARCA_BUCKET_PREFIX` to
`drive/` does not make the old objects readable.**

Drive built a bucket key out of the owner and the path,
`drive/<owner>/<path>`. Arca builds one out of an object id,
`<prefix><shard>/<id>`. There is no id inside an old key to carry over, so
the copy mints a new object id for every distinct key it reads, and those ids
name keys that no bytes lie at yet. The second command puts them there, one
copy per key, inside the bucket. No byte leaves your store.

```sh
go run ./tools/move-objects \
  -manifest manifest.tsv \
  -bucket arca-prod \
  -endpoint https://s3.example \
  -region us-east-1 \
  -path-style \
  -prefix drive/
```

Run it right after the copy, before you switch any route. The credentials come
from `ARCA_BUCKET_ACCESS_KEY` and `ARCA_BUCKET_SECRET_KEY`, or from your
cloud's credential chain when both are unset, so no secret goes on the command
line. `-prefix` has to be the manifest's prefix and `ARCA_BUCKET_PREFIX`, and
the run refuses the command if the three disagree. `-concurrency` defaults to
16 keys at once. `-path-style` is for a store without virtual hosted buckets,
such as MinIO.

Add `-dry-run` first. It reads both ends of every line and writes nothing, so
the counts it prints are the counts the real run will print.

**The byte check is on, and you have to turn it off rather than on.** After
each copy, and for each key a resumed run skips, the object is read back out
of your bucket, hashed, and compared to the checksum the row carried. That is
the only check that holds at a store which reports no checksum of its own,
which is every S3 store we know of, DigitalOcean Spaces and MinIO among them:
without it a copy is proved by its length. It costs one read of every byte you
move, so the run takes about as long as reading your bucket once.

| Flag | Default | What it does |
|---|---|---|
| `-verify-bytes` | on | read each destination back and hash it |
| `-verify-bytes-max` | 268435456 (256 MiB) | read back in full every object at or under this size |
| `-verify-sample` | 10 | the percentage of the larger objects to read back, chosen by hashing the key, so a rerun reads the same ones |

Turn it off only when you are rehearsing against a copy of your data.

```
outcome     keys  means
copied      812   copied to the object id's key and read back
skipped     0     the destination already held the bytes, so this run left it alone
mismatched  0     the destination holds other bytes, and nothing was overwritten
failed      0     the store could not answer for the key

noted
  verified on bytes  807  read back from the store and digested to the checksum the row
                          carries
  verified on size   5    the size and the store's own copy are what hold: the row's
                          checksum is a digest no store reports, and this run did not
                          read the object back
```

- **copied** is the objects now readable at their object id's key.
- **skipped** is the destinations that were already right. A killed run
  resumes and a finished run repeats: rerun the same command as often as you
  like.
- **mismatched** is a destination holding something else, a destination whose
  bytes hash to something else included. Nothing is overwritten and every key
  is named. Look at each one before you continue.
- **failed** is a key the store could not answer for, a missing source among
  them. Every key is named with the reason.
- **verified on bytes** is the objects read back and hashed. This is the count
  that makes the move provable; aim for all of them.
- **verified on label** appears where the row's checksum is a label your store
  reports for a whole object, which is compared without reading the bytes.
- **verified on size** counts what neither check could reach: an object above
  the threshold that the sample did not pick, a row whose checksum is the
  composite label of a multipart upload, or every row if you turned the byte
  check off. Read this number. It is how many objects you are taking on
  trust.
- **publicity not stamped** appears when your store has no object ACLs and
  serves public objects through a bucket policy instead. Those objects are
  copied; only the stamp is the policy's job.

The run exits 0 when nothing mismatched and nothing failed, 1 when anything
did or the command was refused, and 2 on a bad flag. Without
`-delete-sources` it deletes nothing: the old keys stay where they are, and
the manifest is what tells you which they were.

Both halves have to hold before the routes switch: the copy's report and this
one. Spec 019 in this repository records why.

## Deleting the old keys

After the move, every object is in your bucket twice: under the old key and
under the key its object id derives. When you retire the old service, and not
before, the same command deletes the old ones.

```sh
go run ./tools/move-objects \
  -manifest manifest.tsv \
  -bucket arca-prod \
  -endpoint https://s3.example \
  -region us-east-1 \
  -path-style \
  -prefix drive/ \
  -delete-sources
```

The flag adds a pass of its own after the move. The move runs first and
verifies every destination exactly as it did before; only then is each source
key deleted, one key per request, and only where its destination held. A
source whose destination mismatched, failed, or is not in the bucket is kept
and named: the bytes under it are your only copy of that object, and the run
exits 1 so that you look at it.

```
sources
  deleted drive/u-1/files/notes.md
  kept drive/u-1/files/logo.png: drive/20/b did not verify: the destination holds 3 bytes and the manifest says 10

1 source key deleted, 1 kept, 0 the store would not delete
```

- Add `-dry-run` first. It names every key it would delete and calls nothing.
- The manifest is the only list. No key outside its first column is touched,
  which is what makes this safe to run against a bucket Arca is already
  serving from.
- `-verify-bytes=false` is refused with this flag. A delete leaves one copy of
  the bytes, and a length is not a proof to leave it on.
- Rerunning is safe: a destination that is already right is skipped, and a key
  that is already gone deletes cleanly.

Run it only after a person has read an object through the new service. Until
then, a byte the move got wrong is still under its old key.
