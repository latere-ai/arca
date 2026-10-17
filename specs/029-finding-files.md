---
title: "Finding files: a folder listing narrowed to names that contain a text, and the newest files under a folder"
status: in-progress
track: core
depends_on:
  - specs/005-files.md
  - specs/013-api.md
  - specs/028-one-tree.md
affects: [internal/files/, internal/store/, internal/uploads/, api/openapi.yaml, test/e2e/, docs/api.md, specs/005-files.md, specs/013-api.md]
effort: small
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# Finding files

## Overview

### Scope

Two query parameters on the folder listing of [[005-files]],
`GET /v1/files/{owner}/{path...}?list=1`: `q`, which keeps the files
whose name contains a text, and `recent`, which answers the newest files
under the folder. Both ask the question the listing asks and answer the
listing's shape. No route, no action, no table and no migration is added.

### Problem

A client that lets a person pick one of their files by typing part of its
name, or offers the files they touched last, has two ways to do it today.
It reads the whole tree a page at a time and filters it, which costs a
round trip per thousand files on every keystroke, or it keeps an index of
its own, which goes stale the moment another client writes. The rows that
answer both questions are already in the metadata store, under the same
authorization the listing reads.

## Design

### The parameters

Both are read on a request that carries `?list=1`, beside `?limit=` and
`?cursor=`.

| Parameter | Value | Answer |
|---|---|---|
| `q` | a text of at most `files.MaxQueryBytes` (200) bytes | the live files under the path whose name contains the text, ignoring case, in path order and paged by the path cursor exactly as the plain listing is |
| `recent` | `0` or `1` | with `1`, the live files under the path, newest `modified` first, at most `limit` of them, in one page |

A file's name is the last segment of its path. A folder is not matched by
its own name, because Arca stores no folder: a file under `files/plan/`
matches `plan` only if its own name holds the text.

`recent=1` with a `q` is the newest files whose name contains the text.
The order is `modified`, newest first, and two files modified at the same
instant are in path order, so a page is the same page twice. `modified`
is the row's own time, which a move sets too: a file moved a minute ago
is among the newest.

The answer is the listing's `Listing`, with these members:

| Member | Search (`q`) | Recent (`recent=1`) |
|---|---|---|
| `entries` | the matches of this page | at most `limit` files |
| `prefixes` | `[]`: the folders a page of matches implies are not the folders of the tree | `[]` |
| `next_cursor` | the last path of the page when a further page exists | never present |
| `space` | never present, at the plane root too | never present |

`space` is the counter a space is charged against and answers what the
space holds, which neither question asks; a picker that asks on every
keystroke does not pay the ledger read either.

### What the text matches

- **Literally.** `%`, `_` and `\` in `q` match themselves. The text is
  escaped into the `LIKE` pattern, as a prefix already is.
- **Ignoring case.** Both sides pass through Postgres `lower()`. Beyond
  ASCII, folding is the database's own: a database whose `LC_CTYPE` is a
  UTF-8 locale, which is the default of the official image, folds `É` to
  `é`, and a database created with the `C` locale folds ASCII only. No
  collation is pinned in the statement, because the ICU collation that
  would make folding independent of the locale is optional in the
  Postgres 16 and later that [[004-metadata-store]] admits, and a
  statement that names a missing collation fails every search.
- **Without normalization.** A name stored decomposed and a text typed
  composed are two strings.
- **Blank is no search.** A `q` that is empty or only white space is the
  plain listing, with its `prefixes` and, at the plane root, its `space`.
  A `q` that holds text is matched as sent, white space included.

### Authorization

The request asks one `file.list` about the path, as the plain listing
does, and reads the answer the same way:

- an answer whose list filter does not admit the space is an empty page,
  never a `403` ([[006-identity]]);
- every subtree `limits.omit` names is left out ([[028-one-tree]]). The
  omission is in the statement, so a page of matches is never short for a
  file it left out, a cursor resumes on a file the caller read, and
  `recent` counts its `limit` over what may be read.

Arca keeps no folder rules of its own. A folder an installation reserves
for its own use is shaped by its authorizer's answer, which these
parameters read exactly as the plain listing reads it.

### Refusals

| Request | Answer | `fields` |
|---|---|---|
| `q` longer than 200 bytes, not valid UTF-8, or holding a NUL | `400` `invalid_field` | `q` |
| `recent` other than `0` or `1` | `400` `invalid_field` | `recent` |
| `recent=1` with a `cursor` | `400` `exclusive_fields`: a recent page is the whole answer and has no next page | `recent`, `cursor` |

They are read before the question is asked, as `limit` is.

### The statements

Two statements in `internal/store/files.go`, beside `ListPrefix` and over
the same columns:

| Statement | Selects | Order | Page |
|---|---|---|---|
| `SearchPrefix` | live rows under the prefix, after the cursor, whose last segment, lowered, is `LIKE` the escaped text, lowered, within `%…%` | `path` | `LIMIT` the handler's `limit+1` |
| `RecentPrefix` | live rows under the prefix, and with a text, the same name match | `updated_at DESC, path` | `LIMIT` the handler's `limit` |

Both carry the omission predicate every enumerating statement carries.
Neither needs a migration. `files_prefix_idx` narrows a statement to the
subtree, and the name match and the sort run over the rows of that
subtree, which is a few thousand in a space today. When a space grows past
that, a trigram index on the name and an index on `(owner, updated_at)`
can be added without a change to this contract.

### Not in this spec

- Matching inside a file's bytes, or on its content type.
- A search across spaces: the question names one space.
- A cursor for `recent`. A page of the newest files is what a picker
  shows; a client that needs every file reads the plain listing.
- A search on the link listing of [[008-shares-and-links]].

## Acceptance criteria

| # | Criterion | Proved by | State |
|---|---|---|---|
| 1 | `q` answers the live files under the path whose last segment contains the text ignoring case, in path order, paged by the path cursor, with `prefixes` `[]` and no `space` at the plane root; a sibling of the path and a trashed file are absent | `internal/files` `TestASearchKeepsTheNamesThatHoldTheText`; `internal/store` `TestStoreASearchMatchesTheNameUnderThePrefix` | |
| 2 | `%`, `_` and `\` in `q` match themselves and nothing else | `internal/store` `TestStoreASearchTakesTheTextLiterally`, `TestASearchBindsTheTextEscaped` | |
| 3 | A non-ASCII name is matched ignoring case on a UTF-8 database | `internal/store` `TestStoreASearchFoldsTheCaseOfANameBeyondASCII` | |
| 4 | Nothing under a subtree `limits.omit` names is in a search or a recent page, matches inside it included, and a page stays full | `internal/files` `TestASearchAndRecentLeaveOutWhatTheAnswerOmits`; `internal/store` `TestStoreAnOmittedSubtreeIsLeftOutOfASearchAndRecent`, `TestAnEnumerationCarriesTheOmissionInItsStatement` | |
| 5 | A blank `q` is the plain listing, with its `prefixes` and its `space` | `internal/files` `TestABlankSearchIsThePlainListing` | |
| 6 | `q` over 200 bytes, not UTF-8 or with a NUL is `invalid_field` on `q`, `recent` other than `0` or `1` is `invalid_field` on `recent`, and `recent=1` with a `cursor` is `exclusive_fields`, each before the authorizer is asked | `internal/files` `TestASearchOrRecentItCannotReadIsRefused` | |
| 7 | `recent=1` answers the newest `modified` first with ties in path order, at most `limit`, no `next_cursor`, no trashed file, and with `q` only the matches | `internal/files` `TestRecentAnswersTheNewestFilesFirst`; `internal/store` `TestStoreRecentIsNewestFirstAndBounded` | |
| 8 | Both ask one `file.list` about the path, and an answer whose filter does not admit the space is an empty page | `internal/files` `TestASearchAndRecentAskTheListingsQuestion` | |
| 9 | Both statements carry the subtree and the omission, and every failure of the query, the scan and the walk reaches the caller | `internal/store` `TestASearchAndRecentSurfaceEveryWayTheyCanFail`, `TestAnEnumerationCarriesTheOmissionInItsStatement` | |
| 10 | `arcad` as a process answers both against the real stores | `test/e2e` `TestE2EAFolderIsSearchedByNameAndListedNewestFirst` | |
| 11 | The `GET` row of the document names both parameters, and the committed document is current | `tools/apidoc` `TestTheCommittedDocumentIsCurrent` | |
