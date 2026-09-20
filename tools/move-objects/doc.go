// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command move-objects copies every object a manifest names to the key its
// object id derives, which is the second half of step 3 of the cutover of
// spec 019.
//
// Drive built a bucket key from an owner and a path and Arca derives one from
// an object id (spec 003), so the row copy mints an id per distinct key and
// leaves the bytes where they were. This command moves them, one server side
// copy per key, inside one bucket: no byte crosses the network twice.
//
//	move-objects \
//	  -manifest manifest.tsv \
//	  -bucket arca-prod \
//	  -endpoint https://s3.example \
//	  -region us-east-1 \
//	  -prefix drive/
//
// It runs after `migrate-drive -manifest <path>` and before the routes
// switch. It refuses a manifest without its trailer, which is a copy that was
// killed or did not verify, and it refuses a prefix that disagrees with the
// manifest's header or with ARCA_BUCKET_PREFIX, because the copy is read
// under one prefix and served under another. The credentials come from
// ARCA_BUCKET_ACCESS_KEY and ARCA_BUCKET_SECRET_KEY, or from the SDK's chain
// when both are unset, so no secret reaches a command line.
//
// What it does per line:
//
//   - Reads the destination. One that already holds the line's size and
//     checksum is a skip, so a killed run resumes where it stopped and a
//     second run of a finished move writes nothing. One that holds other
//     bytes is a mismatch and is never overwritten.
//   - Copies the source to the destination. A source above the API's single
//     copy maximum goes range by range.
//   - Reads the destination back and compares it against the line. Size
//     always. Then the bytes: the destination is streamed through a sha256
//     and compared to the line's checksum. That is the one proof a store
//     reporting no checksum of its own can give, and every store the family
//     runs is such a store. A line whose checksum is a label the store
//     reports takes that comparison instead, and one that is neither, which
//     is the composite label of an object assembled from parts, is counted
//     and named as verified on size alone.
//   - The byte check is on by default and bounded: every object at or under
//     -verify-bytes-max, and -verify-sample percent of the larger ones,
//     chosen by a digest of the key so two runs choose the same keys and a
//     resumed run leaves no hole. -verify-bytes=false turns it off, which is
//     an opt out and never an opt in: a clean report nobody read a byte for
//     is the failure this command exists to catch.
//   - Stamps a public destination, because a copy carries no ACL. A store
//     that holds no object ACLs and offers bucket policies instead is
//     counted rather than failed, which is the rule of spec 003.
//
// It never deletes without -delete-sources. The source keys stay until the
// sunset of spec 019, and that step is this same command with the flag:
//
//	move-objects -manifest manifest.tsv -bucket … -delete-sources
//
// The flag adds a pass of its own, which begins after the move has read back
// and verified every destination the manifest names. That pass deletes the
// source key of each destination that held, one DeleteObject per key, and
// keeps and names every source whose destination mismatched, failed, or is
// not in the bucket, because the bytes under such a source are the only copy
// left. It reports `deleted <source>` per key and counts them at the end. A
// dry run names what it would delete and calls nothing.
//
// The byte check has to be on for it: -delete-sources with
// -verify-bytes=false is refused, because a length is not a proof to delete
// the other copy of an object on.
//
// The report counts the keys copied, skipped, mismatched and failed, and
// names every key of the last two. It exits 0 when nothing mismatched,
// nothing failed and no delete was refused, 1 when anything did or the run
// was refused, and 2 on a bad flag, which is what migrate-drive exits and
// what an operator scripts the two around.
//
// With -dry-run it reads the destination and the source of every line and
// writes nothing, so the counts a rehearsal prints are the counts the run
// will print.
package main
