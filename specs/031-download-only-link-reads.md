---
title: "Download-only link reads: every answer of the three link routes is an attachment that is never sniffed and never runs, a link's redirect asks the bucket for the attachment, and a file's name is encoded so that no name can break the header"
status: complete
track: core
depends_on:
  - specs/003-object-store.md
  - specs/005-files.md
  - specs/008-shares-and-links.md
  - specs/013-api.md
affects: [internal/api/, internal/files/, internal/blob/, internal/shares/, api/openapi.yaml, test/conformance/, test/e2e/, docs/api.md, docs/configuration.md, SECURITY.md, specs/003-object-store.md, specs/005-files.md, specs/008-shares-and-links.md, specs/013-api.md, specs/015-security-and-threat-model.md]
effort: small
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# Download-only link reads

## Overview

### Scope

The three routes that redeem a link token ([[008-shares-and-links]])
answer every request with three headers:

```
Content-Disposition: attachment
X-Content-Type-Options: nosniff
Content-Security-Policy: sandbox
```

The file route names the file in the disposition, encoded so that any
name a path may hold is carried exactly and none can end the header or
add another. A link read that redirects asks the bucket, through the
signed URL, to answer with the same disposition. No route, no action, no
table and no setting is added.

### Problem

A link is a URL anyone may open with no token of their own, and a
browser that opens it renders what comes back as a page of the origin
that served it. Arca stores what its callers write, and one class of
caller writes files that no person reviewed: a program's output, a page
an agent produced, a report built from untrusted input. A consumer that
publishes such files through links cannot let a browser render one as a
document of the installation's origin, where its script would run beside
everything else that origin serves.

Today a link read answers the stored `Content-Type` and nothing that
stops a browser from rendering it. An HTML file read through a link is a
page of the origin; a file stored as `text/plain` may be sniffed into
one. The redirect above `ARCA_INLINE_BYTES` sends the browser to the
bucket with no disposition unless the caller asked for `?download=1`,
and a public object is sent to `ARCA_PUBLIC_CDN_URL`, where no
disposition can be asked for at all.

A consumer can only promise that its published files are downloaded and
never rendered if the store makes that true for every link read. This
spec makes it true, so the promise is the store's and not each
consumer's.

## Design

### The three headers

| Header | Value | What it stops |
|---|---|---|
| `Content-Disposition` | `attachment`, and on the file route `attachment` with the file's name | a browser that navigates to the URL, or a frame that loads it, rendering the answer: it is saved instead |
| `X-Content-Type-Options` | `nosniff` | a browser reading the bytes as a type other than the one stated, such as a `text/plain` file sniffed into HTML or loaded as a script |
| `Content-Security-Policy` | `sandbox` | the answer, if it is ever rendered as a document, running script, submitting a form, or acting as the origin: it is a document of a unique opaque origin |

The values are exactly these. `sandbox` with no allow-list is the most
restrictive form; a consumer that compares the header with the string
`sandbox` finds it.

They are on every answer of the three routes: `200`, `302`, `304`, every
refusal, and the `not_implemented` of a build that binds no shares
service. The frame's dispatcher for the three rows, `(*API).link` in
`internal/api`, sets them before the handler writes anything, so a route
cannot answer without them and a refusal written by any layer below
carries them. `Referrer-Policy: no-referrer` stays beside them, set by
the handlers as before.

The meta and the listing are JSON, which no browser runs. They carry the
headers anyway: the rule is a property of the three routes, not of a
content type, so no later change to an answer's shape can leave one
route out. A client that reads them with `fetch` or any HTTP library is
unaffected, since neither reads `Content-Disposition` or a response's
policy.

### The file's name

On the file route, the disposition names the last segment of the path
the request named, the name a person expects the download to be saved
under:

```
Content-Disposition: attachment; filename="q3 report.pdf"; filename*=UTF-8''q3%20report.pdf
```

It is set once the object is resolved and before the conditional check,
so a `304` carries the same disposition as the `200` it revalidates and
a cache that refreshes its stored headers from the `304` keeps the name.

`api.Attachment` renders it, the one place a name enters a header:

1. The name is cleaned. Invalid UTF-8 is dropped, every control
   character (`unicode.IsControl`, which includes CR, LF and NUL) is
   removed, and so is every format character (`Cf`, such as U+202E,
   which reverses how the rest of a name reads and can hide its real
   extension) and every line or paragraph separator (`Zl`, `Zp`).
   [[005-files]] refuses C0 controls in a path already; the encoder
   does not rely on it, because a name is a header's business here.
2. A name longer than 255 bytes, the longest file name the common file
   systems take, or one with nothing left but dots and spaces, is saved
   as `download`.
3. `filename*` carries the cleaned name in UTF-8, percent-encoded per
   RFC 8187 (which obsoletes RFC 5987): every byte outside `attr-char`
   is `%XX`. A browser that reads it prefers it, as RFC 6266 section
   4.3 says.
4. `filename` is the fallback for a reader that takes no other form: a
   quoted string in ASCII where each byte outside printable ASCII, and
   each `"`, `\` and `%`, is `_`. RFC 6266 appendix D advises against a
   backslash, which some agents do not unescape, and a percent sign,
   which some decode; replacing them leaves the quoted string with
   nothing to escape.

Nothing in either form can end the parameter or the header: `filename`
holds no quote and no control character, and `filename*` holds only
`attr-char` and `%XX`. `mime.ParseMediaType` decodes the header back to
the cleaned name.

### The redirect

Above `ARCA_INLINE_BYTES` a read is a `302` to a presigned URL
(invariant 4 of [[001-architecture]]). The browser follows it to the
bucket, and the headers of Arca's `302` do not travel with it. So a link
read always presigns with the attachment disposition, the same string
the streamed answer carries, whatever `?download=` says, and the bucket
answers the object with `Content-Disposition: attachment` and the name
(`response-content-disposition`, [[003-object-store]]).

A signed URL can ask the bucket for a disposition and cannot ask it for
`X-Content-Type-Options` or `Content-Security-Policy`; the S3 API has
no override for either. What keeps the redirected read download-only is
the disposition: a navigation to it and a frame that loads it save the
file. The bytes are then on the bucket's host, which is not the server's
origin. An installation that puts the bucket behind a host it also
serves pages from sets the two headers on that host's answers, which
`docs/configuration.md` tells the operator beside the bucket's settings.

A public object read through a link is presigned the same way and is
not sent to `ARCA_PUBLIC_CDN_URL`. The CDN URL is unsigned, a store
refuses response overrides on an anonymous read, and the CDN would
answer the object under the type it was written with, rendered on the
CDN's host. The object stays public in the bucket: a `public` grant
still marks it, and whoever holds its direct URL reads it there. What
changes is that the link route does not hand that URL out. An owner's
own read of a public object is redirected to the CDN as before, and an
owner's `?download=1` read of one is presigned with the disposition,
because a download that goes to the CDN cannot be a download.

### An owner's own read

`GET /v1/files/{owner}/{path...}` keeps the stored type inline and
gains `X-Content-Type-Options: nosniff` on every answer that carries
bytes or their headers (the streamed `200` and the `HEAD`). It is not an
attachment and is not sandboxed by default, and `?download=1` is how the
owner asks for the attachment.

The reasons:

- **A browser cannot open it.** The route is behind the verifier, which
  reads a bearer from the `Authorization` header and from nowhere else:
  no cookie, no query parameter ([[006-identity]]). A navigation, a
  frame and an `img` element send no such header, so the only client
  that receives an owner's read is code that already holds the owner's
  token and chose to fetch it. What that code does with the bytes, a
  preview or a save, is its decision, and a proxy that turns a session
  into a bearer for a browser sets its own headers on what it relays.
  The link routes are the only reads a URL alone can open, which is why
  the rule is theirs.
- **The attachment would break what an owner's client does with its own
  files.** A client previews an image or a PDF by framing the redirect
  or the streamed answer; an attachment would turn each preview into a
  download, and a sandbox stops a browser's PDF viewer.
- **`nosniff` costs nothing.** It never changes how a client that reads
  the stated type behaves, and it stops a browser from guessing a type
  the owner did not state.

`?download=1` now puts the disposition on the streamed answer as well as
on the presigned URL, so it means the same at every size. Before this
spec it changed only the redirect, and a download at or below the inline
size was answered inline.

### Compatibility

| Who | What changes for them |
|---|---|
| A browser that opens a link's file | the file is saved under its name, whatever its type; before, an HTML or a sniffable file rendered as a page of the server's origin |
| A page that frames a link's file, such as a PDF preview | the frame saves the file rather than showing it. An `img` element and a `fetch` read the bytes as before, since neither reads the disposition |
| A client of a `public` link to one object | the file route redirects to a presigned URL with the disposition, not to `ARCA_PUBLIC_CDN_URL`; the object stays public in the bucket |
| An owner's read | `nosniff` on the streamed answer and the `HEAD`; `?download=1` at or below the inline size is an attachment; `?download=1` on a public object is presigned, not sent to the CDN |
| A name in a presigned disposition | rendered by `api.Attachment` in both forms; before, Go's quoting put raw UTF-8 or Go escapes in the header |
| `blob.PresignOptions` | `Filename` is replaced by `Disposition`, the whole header value, so the store never renders a name |
| The previous release's conformance suite | no case asserts what changes: its link file case reads `?inline=1` and checks the status and the bytes |

### Not in this spec

- `X-Content-Type-Options` and `Content-Security-Policy` on the bucket's
  or the CDN's own answers, which a signed URL cannot ask for.
- A setting that turns the rule off. A link that renders is the defect
  this spec removes, and an installation that wants a page for its links
  renders one on its own origin over the three routes, as
  [[008-shares-and-links]] says.
- A change to the content type a link read answers. The stored type is
  kept, so an `img` element, a media element and a `fetch` read the
  bytes as what they are.

## Acceptance criteria

| # | Criterion | Proved by | State |
|---|---|---|---|
| 1 | Every answer of the three link routes carries `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff` and `Content-Security-Policy: sandbox`: the meta, the listing, the file, a refusal of each, and the `not_implemented` of a build with no shares service | `internal/api` `TestEveryAnswerOfTheLinkRowsIsDownloadOnly`, with a bound service and with none; `internal/shares` `TestEveryAnswerOfTheLinkRoutesIsDownloadOnly`, over the three routes, an unknown token on each, and a path outside the prefix | built |
| 2 | The file route's disposition names the path's last segment in both forms, set before the conditional check, so a `304` carries it | `internal/files` `TestALinkReadIsAnAttachmentUnderItsName`, `TestALinkRevalidationKeepsTheName` | built |
| 3 | `api.Attachment` leaves no control character, no format character and no unescaped quote in the header for any name; it falls back to `download` for an empty, dot-only or over-long name; and `mime.ParseMediaType` decodes its `filename*` back to the cleaned name, for ASCII, CJK, accented and right-to-left names | `internal/api` `TestAnAttachmentCarriesTheNameExactlyAndNothingElse`, and `FuzzAttachment`, whose seeds run with the unit tier | built |
| 4 | A link read above the inline size presigns with the disposition whatever `?download=` says, and a link read of a public object presigns rather than redirecting to `ARCA_PUBLIC_CDN_URL` | `internal/files` `TestALinkReadThatRedirectsAsksTheBucketForTheAttachment`, `TestALinkReadOfAPublicObjectIsPresignedAndNotSentToTheCDN` | built |
| 5 | The bucket answers a presigned read with the disposition the URL asked for, a non-ASCII name included | `internal/blob` `TestStoreAPresignedReadIsOneKeyAndOneMethod` against MinIO | built |
| 6 | An owner's read carries `nosniff`, no disposition and no policy by default; `?download=1` is an attachment at or below the inline size and presigned with the disposition above it and for a public object | `internal/files` `TestAnOwnersReadIsInlineAndNeverSniffed`, `TestALinkReadOfAPublicObjectIsPresignedAndNotSentToTheCDN`, `TestAVersionDownloadIsAnAttachment` | built |
| 7 | `arcad` as a process answers a link's file, meta and listing with the three headers and the named disposition | `test/e2e` `TestE2EALinkIsReadForDownloadOnly`, which also follows the redirect to MinIO and reads the disposition the bucket answers | built |
| 8 | The portable suite holds an installation's link file route to the three headers | `test/conformance` `case008LinkFile` | built |

## Outcome

Complete on 2026-10-18. The headers are set by `DownloadOnly` in
`internal/api/disposition.go`, called first in the frame's dispatcher
`(*API).link`, so every answer of the three rows carries them, the
`not_implemented` of a build with no shares service included. The name
is rendered by `api.Attachment` in the same file. `internal/files`
`ServeObject` is the link read: `serve` sets the named disposition
before the conditional check when the read is a link's, and `attachment`
makes a link read a download whatever `?download=` says, which sends a
public object to `presign` rather than to the CDN. `blob.PresignOptions`
carries `Disposition`, the rendered header, in place of `Filename`, and
`blob.S3` and `blob.Memory` pass it through. `headers`, which a streamed
read and a `HEAD` share, sets `nosniff`. Every criterion has a passing
test in its tier. Criterion 5 runs in the store tier, criterion 7 in the
e2e tier and criterion 8 in the conformance tier, which the gate does
not run; all three were run against the local stack for this change
(`make test-store` for `internal/blob` and `internal/files`,
`make test-e2e`, `make test-conformance`). `FuzzAttachment` ran for
twenty seconds, a little over a million names, with no failure.

[[015-security-and-threat-model]] gains the control "a file a link
serves rendered as a page of the server's origin" and criterion 27, and
`SECURITY.md` the commitment that a file a link serves is saved as a
download and never rendered as a page of the server's origin, which the
mapping of that spec answers with the new control.

Built where the design was silent, each a choice to review:

- **The headers are the frame's, not the handlers'.** Setting them in
  `(*API).link` rather than in each of the three handlers of
  `internal/shares` covers a refusal written by any layer below and a
  build that binds no shares service. `Referrer-Policy` stays where it
  was, in the handlers.
- **The fallback name is `download`.** A name with nothing left once it
  is cleaned, or longer than 255 bytes, is saved under it. A long name is
  not cut, because a cut name loses its extension.
- **The ASCII stand-in replaces a character, not a byte.** `résumé.pdf`
  falls back to `r_sum_.pdf`; `filename*` carries the name exactly.
- **A link read keeps the stored type.** It is not rewritten to
  `application/octet-stream`, as a page that previews a linked image in
  an `img` element needs `image/svg+xml` for an SVG, and the disposition,
  `nosniff` and the sandbox already keep a browser from rendering it. The
  signed URL asks the bucket for the disposition only, for the same
  reason.
- **An owner's download is a download everywhere.** `?download=1` on a
  streamed read, on a version and on a public object follows the link
  read's rule. Before, it changed only the redirect.
- **An invalid `?download=` on a public object is now refused.** The
  parameter is read before the public branch, which it now steers, so
  `?download=maybe` is `invalid_field` there as it already was for every
  other object.
