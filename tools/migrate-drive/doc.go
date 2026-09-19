// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command migrate-drive copies Drive's metadata into an Arca database, which
// is step 3 of the cutover of spec 019.
//
// Arca owns a fresh database. This command reads Drive's, rewrites what the
// two schemas spell differently, and writes Arca's, one transaction per table
// in dependency order. It writes nothing to the source, so Drive is read-only
// for the length of the run and is not changed by it.
//
//	migrate-drive \
//	  -source postgres://user@host/drive \
//	  -target postgres://user@host/arca \
//	  -issuer https://issuer.example \
//	  -org-subjects orgs.json
//
// What the run rewrites, per spec 019:
//
//   - Every owner column becomes a subject. Drive addressed a space with a
//     pair, (owner_type, owner_id); Arca has one column holding the subject
//     <issuer>|<sub>. A principal takes the issuer of -issuer, and an
//     organization takes the subject the platform's identity provider assigns
//     it, which only the file -org-subjects names knows.
//   - Four planes become two. files/ and workspaces/ stay, memory/ folds into
//     files/memory/, and repos/<name>/ becomes workspaces/<name>/.
//   - Grants whose grantee is a role, a team or an email address do not
//     arrive, and neither do the pending and denied statuses, which are the
//     share requests the platform now owns.
//   - The workspace kind and agent_access do not arrive, and neither do the
//     quotas, webhooks, agent_visibility, admin_audit and share request rows.
//   - space_usage is recomputed from the copied rows rather than copied.
//
// The run refuses before it writes anything: an organization no mapping
// names, a path in a plane spec 019 gives no rule for, two workspaces that
// would collide once the kind is dropped, a key outside -prefix, and a target
// that already holds rows. Afterwards it verifies row counts and a sample of
// checksums and exits 1 on any mismatch. With -dry-run it reads, rewrites and
// reports without opening a transaction.
//
// The bytes are a separate matter and the report says so. Drive's
// storage_key carries an owner and a path where Arca's key derives from an
// object id, so this copy cannot leave the bytes addressable where they lie.
// Spec 019 records the finding.
package main
