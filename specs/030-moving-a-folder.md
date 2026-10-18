---
title: "Moving a folder: every object under a folder renamed to the same path below a destination, in one call and one transaction, with no byte copied"
status: complete
track: core
depends_on:
  - specs/003-object-store.md
  - specs/005-files.md
  - specs/006-identity.md
  - specs/008-shares-and-links.md
  - specs/010-events-and-reaper.md
  - specs/013-api.md
  - specs/028-one-tree.md
affects: [internal/files/, internal/store/, authorizer/, internal/api/, api/openapi.yaml, test/conformance/, test/e2e/, docs/api.md, specs/005-files.md, specs/006-identity.md, specs/010-events-and-reaper.md, specs/013-api.md, specs/028-one-tree.md]
effort: medium
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# Moving a folder

## Overview

### Scope

`move_to` takes `recursive` the way `copy_to` does. With
`"recursive": true` the source of the `POST` is the folder at the path,
and every live object under it moves to the same path below `move_to`,
in one call and one database transaction. No route, no action, no table
and no migration is added. [[028-one-tree]] left this out ("a move stays
one object"); this spec adds it.

### Problem

A client that renames a folder today moves each object under it with a
request of its own. A folder of a thousand files is a thousand round
trips, each asked and logged by itself, and a failure halfway leaves
the folder split between two paths with nothing to say which half is
where. A folder copy already runs in one call; a rename, which needs no
byte copied at all, cannot.

## Design

### A folder move is a rename of rows

A bucket key derives from an object id and never from a path
([[003-object-store]], key derivation: `<ARCA_BUCKET_PREFIX><shard>/<object id>`,
built by `object.ID.Key`; [[001-architecture]] invariant 8). Every row
under the folder keeps its id, its object id, its size, its checksum and
its creation time, and changes its path. The bucket is not called, the
space's usage does not move, and nothing is copied. Migration
`0006_one_tree.up.sql` of [[028-one-tree]] moved every workspace into a
folder by the same rename.

### The request

```
POST /v1/files/{owner}/{path...}
{"move_to": "files/archive/2026", "recursive": true}
```

`path` names a folder: every object whose path starts with `path/`, and
never the object at exactly `path`, as for the folder delete and the
copy of [[028-one-tree]]. Each object under it moves to `move_to/` and
the rest of its path: `files/notes/deep/b.md` moves to
`files/archive/2026/deep/b.md`.

Without `recursive` a move is one object, as [[005-files]] has it.

### The question

One `file.write`, with the destination as `path`, the source as `from`,
`recursive: true`, and `size` the sum of the live bytes under the folder,
counted before the answer and so before any omission. It carries no
`id` and no `copy`: a question that carries `from` without `copy` is a
move ([[006-identity]]). An authorizer that does not read `recursive`
answers it as the move of one path, which is never wider than what was
asked. `grant` is the lower of the caller's rungs on the two paths, as
on every question with `from`.

### What moves, and what stays

| | Moves | Stays |
|---|---|---|
| Objects | every live object under the folder that the answer's `limits.omit` covers at neither end | a trashed object, the object at the folder's own path, and an object left out by `limits.omit` |
| Versions | the history of each moved object, with it | the history of what stays |
| Stars | a star on a moved object follows it; a subject that had starred the destination path keeps one star, as a move of one object leaves | stars on what stays |
| Grants and links | a grant or a link whose prefix is the folder or under it follows to the same prefix below `move_to` | one whose prefix is at, under or above a subtree `limits.omit` covers, at either end; one whose prefix is the path of an object that stays (a trashed one, or the object at the folder's own path), which goes on naming that object; and one whose new prefix would cover a row, live or trashed, that the move did not write |
| Publicity | a public object stays public: publicity is a setting on the bucket key ([[008-shares-and-links]]), and the key does not change | |
| Upload sessions | | a session open under the folder is left alone. One that completes later writes its object at the session's path, by the answer it gets then, as under a folder delete |

A grant follows the folder for the reason [[005-files]] carries an
exact-path grant with a moved object: left behind on a path that is now
free, it would cover the next object written there, which its owner
never shared. A grant on an ancestor of the folder covers a subtree and
stays where it is, as on the move of one object.

A grant never comes to cover more than it did. The destination may be a
folder that already holds objects, or an ancestor of the source
(`files/notes/deep` moved to `files/notes`), and a grant carried onto
such a prefix would hand its grantee objects nobody shared with them. So
a grant whose new prefix covers any row the move did not write stays
where it is, and the moved objects leave what it covers. This is the
rule migration `0006_one_tree.up.sql` of [[028-one-tree]] kept for a
grant on the removed plane: a move narrows access and never widens it.

A trashed object stays at its path with its history. It is in the trash
by itself and is restored to the path it was trashed from, as each
object under a folder delete is.

### Refusals

Checked in this order. Nothing is moved on any of them.

| Request | Answer | `fields` |
|---|---|---|
| `recursive` with `restore_version`, or with no operation | `400` `invalid_field` | `recursive` |
| `path` is `files`, the root of the tree | `400` `invalid_path` | `path` |
| `move_to` is not a path under `files/` | `400` `invalid_path` or `unknown_plane` | `move_to` |
| `move_to` is the folder itself | `400` `invalid_field` | `move_to` |
| `move_to` is inside the folder | `400` `invalid_field` | `move_to` |
| nothing live under the folder | `404` `not_found` | |
| the authorizer refuses | `403` `forbidden` on the caller's own space, `404` `not_found` on another's ([[006-identity]]) | |
| more than `files.MaxCopyObjects` objects | `422` `too_many_objects`; the caller moves the folder's subfolders | |
| every object under the folder is left out by `limits.omit` | `404` `not_found`, as a folder with nothing under it | |
| a row, live or trashed, holds a destination path | `409` `path_taken`, naming the first such path | |
| an object read for the move changed before it was renamed | `412` `precondition_failed`; the caller retries | |

A destination is taken by a trashed row too, because a path holds one
row ([[004-metadata-store]] `UNIQUE (owner, path)`) and the move of one
object is `path_taken` onto a trashed path already. A destination held
by an object this move would itself move away is taken as well: the
destination may be an ancestor of the folder (`files/notes/deep` moved
to `files/notes`), and an object at `files/notes/deep/x` holds the path
`files/notes/deep/deep/x` would move to. Refusing it keeps the answer
the same whatever order the rows are renamed in.

### The bound

`files.MaxCopyObjects`, 10,000 objects in one call, the bound of the
copy, and its refusal `422` `too_many_objects`. The rows are read in one
page of one past the bound, with the source end of `limits.omit` applied
in the statement; a page past the bound is refused before any row
changes, whatever the destination end of the omission would leave out
of it. The same rule holds for the copy.

### Leaving subtrees out

The answer's `limits.omit` shapes the move as it shapes the copy
([[028-one-tree]]): an object is not moved when an entry covers its
source path or its destination path, and `files` and `bytes` leave it
out. An omitted subtree is neither moved nor written into, and a
destination under one is never checked, so a `path_taken` names no path
the caller may not enumerate.

### The transaction

```mermaid
sequenceDiagram
  participant C as client
  participant A as arcad
  participant P as Postgres
  C->>A: POST /v1/files/{owner}/{folder} {move_to, recursive}
  A->>P: count the live rows under the folder and their bytes
  A->>A: authorize file.write once, with path, from, recursive, size
  A->>P: one transaction
  Note over P: read the rows the answer leaves in, one past the bound<br/>refuse a destination any row holds<br/>rename each row, then its versions and its stars<br/>carry the grants and links under the folder
  A-->>C: 200 {path, files, bytes}
```

The rows are renamed only where they are still live at the path that was
read, so an object deleted or moved by another request in between is a
count short and the transaction is refused as `412`. A row written at a
destination between the check and the rename is a unique violation and
`409`. The bucket is not called at any step, and the usage ledger is not
touched.

### The answer and the event

`200` `{"path": "<move_to>", "files": <n>, "bytes": <b>}`, the shape of a
folder copy's answer, where `files` is the objects moved and `bytes`
their sizes.

One `move` event at the destination, appended after the commit, with
detail `{"from": "<folder>", "recursive": true, "files": <n>,
"bytes": <b>}`. The move of one object keeps its detail `{"from"}`.
The tail under `limits.omit` reads both ends of the event, as it does
for every move.

### Compatibility

| Who | What changes for them |
|---|---|
| A client that sent `{"move_to", "recursive": true}` | it was `400` `invalid_field` and is now a folder move |
| A reader of the `too_many_objects` sentence | it names a copy or a move: "The folder holds more objects than one copy or move accepts." |
| An authorizer endpoint | it may be asked a move that carries `recursive: true` and a `size` that is a sum |
| The previous release's conformance suite | its `case028FolderCopy` asserts the `400` above. The case is listed in the superseded cases of [[024-conformance-against-a-published-release]], so the run of that suite against this release skips it and names it, under its reason, in the release body |

### Not in this spec

- Moving a folder between two spaces.
- Moving the trashed objects under a folder with it.
- Moving an open upload session's path.
- A folder move past the bound, which would need a job with a state of
  its own, as for the copy.

## Acceptance criteria

| # | Criterion | Proved by | State |
|---|---|---|---|
| 1 | A folder move renames every live object under the folder to the same path below `move_to`, keeping each row's id, object id, size and checksum, makes no bucket call and leaves the usage as it was | `internal/files` `TestAFolderMoveRenamesTheRowsAndTouchesNoBytes` with `blob.Counting`, and `TestStoreAFolderMoveMakesNoBucketCallAndCarriesWhatKeysOnThePath` against MinIO and Postgres; `internal/store` `TestStoreAFolderMoveRenamesTheRowsAndWhatKeysOnThem` | built |
| 2 | Versions and stars of each moved object follow it; a grant and a link under the folder follow it; a grant on an ancestor stays, and so does one whose new prefix would cover a row the move did not write | `TestAFolderMoveRenamesTheRowsAndTouchesNoBytes`; `TestStoreAFolderMoveMakesNoBucketCallAndCarriesWhatKeysOnThePath`; `internal/store` `TestStoreAFolderMoveRenamesTheRowsAndWhatKeysOnThem`, `TestStoreAFolderMoveLeavesTheGrantsAnOmissionKeeps`, `TestMovingTheGrantsUnderAFolderLeavesWhatStaysBehind`, `TestAFolderMoveCarriesTheStarsAndLeavesOnePerSubject` | built |
| 3 | A trashed object, the object at the folder's own path and a sibling whose name begins with the folder's stay where they are, with their history and an exact-path grant | `TestAFolderMoveRenamesTheRowsAndTouchesNoBytes`; `internal/store` `TestStoreAFolderMoveRenamesTheRowsAndWhatKeysOnThem`, `TestStoreAFolderMoveLeavesTheGrantsAnOmissionKeeps` | built |
| 4 | A destination a live or a trashed row holds, an object this move would move away included, is `path_taken` and nothing moves | `TestAFolderMoveOntoATakenPathMovesNothing`; `internal/store` `TestStoreAFolderMoveRefusesADestinationAnyRowHolds`, `TestTakenNamesTheFirstPathARowHolds` | built |
| 5 | `move_to` equal to the folder or inside it is `invalid_field` on `move_to`; the plane root is `invalid_path`; nothing under the folder is `not_found` | `TestAFolderMoveRefusesWhatIsNotAMoveOfAFolder`, `TestAPostNamesOneOperationAndAPathInThePlane` | built |
| 6 | The move asks one `file.write` with `path`, `from`, `recursive: true` and the sum as `size`, and no `id` and no `copy`; a refusal is the caller's own answer on its space and `not_found` on another's | `TestAFolderMoveAsksOnceWithBothEnds`, `TestAFolderMoveNeedsAGrantOnBothEnds` | built |
| 7 | An object whose source or destination `limits.omit` covers is neither moved nor written into, a grant at, under or above it stays, and the counts leave it out | `TestAFolderMoveLeavesOutWhatTheAnswerOmitsAtBothEnds`; `internal/store` `TestStoreAFolderMoveLeavesTheGrantsAnOmissionKeeps`, `TestMovingTheGrantsUnderAFolderLeavesWhatStaysBehind` | built |
| 8 | A folder past `MaxCopyObjects` is `too_many_objects` before any row changes, for a move and for a copy, whatever the destination omission leaves out | `TestAFolderMoveIsBounded`, `TestAThinnedPageIsNotTakenForTheWholeFolder` | built |
| 9 | An object deleted or renamed between the read and the rename refuses the move as `precondition_failed`, and every store failure is `storage_unavailable` with nothing moved | `TestAFolderMoveThatCannotBeRecordedMovesNothing`; `internal/store` `TestAFolderMoveRenamesLiveRowsAndSaysWhenADestinationIsTaken`, `TestAFolderMoveBindsEachSourceBesideItsDestination` | built |
| 10 | One `move` event at the destination carries `from`, `recursive: true`, `files` and `bytes` | `TestAFolderMoveRenamesTheRowsAndTouchesNoBytes`; `test/e2e` `TestE2EAFolderMoveRenamesItsRowsAndLeavesTheBucketAlone` | built |
| 11 | The statements rename in Postgres what the handler asks, under an omission and at a destination that is an ancestor of the folder | `internal/store` `TestStoreAFolderMoveRenamesTheRowsAndWhatKeysOnThem`, `TestStoreAFolderMoveLeavesTheGrantsAnOmissionKeeps`, `TestStoreAFolderMoveRefusesADestinationAnyRowHolds` | built |
| 12 | `arcad` as a process moves a folder against the real stores: same object ids and keys, the bytes read at the new paths, the history at a new path, usage unchanged, a second move onto the destination refused | `test/e2e` `TestE2EAFolderMoveRenamesItsRowsAndLeavesTheBucketAlone` | built |
| 13 | The `POST` row of the document names the folder move, and the committed document is current | `tools/apidoc` `TestTheCommittedDocumentIsCurrent` and the `POST` row of its labels table | built |
| 14 | The portable suite moves a folder against a running installation, and the previous release's suite, whose folder copy case asserted the refusal this spec removes, passes against this binary with that case skipped under its reason | `test/conformance` `case030FolderMove`; `TestPreviousSuitePasses` with the entry of `supersededCases` ([[024-conformance-against-a-published-release]]) | built |

## Outcome

Complete on 2026-10-18. The handler is `internal/files/folder.go`
`moveFolder`, reached from `post` in `mutate.go` when `move_to` carries
`recursive`. It shares the copy's plan, `folderPlan`, its answer shape,
`folderAnswer`, and its destination rule, `destination`, which now names
the field it read. The statements are `internal/store`
`Files.Taken` and `Files.MoveMany` (`files_subtree.go`),
`Versions.MoveMany`, `Stars.MoveMany`, and `Shares.MoveUnder`, which
shares the predicate `untouched` with `DeleteUnder`. Every criterion above
has a passing test in its tier. The store halves of criteria 1 to 4, 7
and 11 run in the store tier and criterion 12 in the e2e tier,
`make test-store` and `make test-e2e`, which the gate does not run; both
were run against the stack for this change.

That a folder move is a rename of rows was checked in the code, not only
in [[003-object-store]]: `object.ID.Key` builds the key from the prefix,
the shard and the id and from nothing else, and `MoveMany` sets `path`
and `updated_at` and no other column. The tiers hold it from outside: no
bucket call through `blob.Counting`, and in the e2e tier the same object
id and the same key in MinIO before and after.

Built where the design was silent, each a choice to review:

- **Grants under the folder follow it, and never widen.** The move of
  one object carries only a grant on its exact path. A folder move
  carries every grant and link whose prefix is the folder or under it,
  because the folder's path is what the move frees. One at, under or
  above an omitted subtree stays, by `DeleteUnder`'s rule; one on the
  path of an object still there stays with that object; and one whose new
  prefix would cover a row, live or trashed, that the move did not write
  stays, so moving a shared folder into a folder that already holds
  objects keeps the share where it was rather than extending it. The
  alternative, carrying exact-path grants only, leaves a grant on an
  emptied folder to cover whatever is written there next.
- **A trashed object stays.** The request names live objects. A trashed
  one keeps its path, its history and an exact-path grant, and is
  restored where it was trashed from. The alternative, moving it with the
  folder as migration `0006` moved trashed workspace rows, would restore
  it into the folder's new place; it is not built.
- **A destination is taken by any row.** A trashed row at a destination
  is `path_taken`, as for the move of one object, and so is an object of
  this move at a destination when `move_to` is an ancestor of the
  folder.
- **The bound counts the page before the destination omission.** Found
  while building, in the copy, and fixed beside it: the plan read one row
  past the bound, left out the rows whose destination the answer omits,
  and then compared the rest with the bound. A folder a few rows past the
  bound, with a few of its first rows omitted at the destination, fitted
  under it, and the copy wrote the first page and answered as if it were
  the whole folder. The bound is now read off the page the statement
  answers, for the copy and the move alike.
- **`too_many_objects` names a move.** Its sentence is "The folder holds
  more objects than one copy or move accepts." in [[013-api]]'s table,
  the code and the suite's copy of the table.
- **A history at a destination with no row there** is `path_taken`, and
  so is a row written at a destination after the check; neither is a
  `503`.
- **The previous release's suite.** `v0.7.0`'s `case028FolderCopy`
  asserts `invalid_field` for `move_to` with `recursive`, and fails
  against this binary for that one assertion. By the owner's decision of
  2026-10-18 the refusal becomes an operation without a major: the case
  is the first entry of the superseded list of
  [[024-conformance-against-a-published-release]], which the run of that
  suite skips and the release body names. This release's
  `case028FolderCopy` reads the same request on an object as `not_found`,
  and `case030FolderMove` drives the folder move.
