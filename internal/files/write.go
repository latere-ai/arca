// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// Write is one content landing at one path: what a put streamed through the
// server, or what a session of spec 007 assembled in the bucket. The bytes
// are already written when a Write is committed, at a key nothing else
// references, so the transaction below is metadata alone.
type Write struct {
	// Owner is the space, Path the path, and Plane the plane it is rooted
	// in.
	Owner, Path string
	Plane       object.Plane
	// Actor is the subject writing, recorded as the creator of a path that
	// did not exist.
	Actor string
	// Object names the bytes this write put in the bucket.
	Object object.ID
	// Size, Checksum, ChecksumKind and ContentType are what the store holds
	// about them.
	Size         int64
	Checksum     string
	ChecksumKind object.ChecksumKind
	ContentType  string
	// Pre is what the request asked of the path's current state.
	Pre api.Precondition
	// Limit is the allowance the authorizer's answer carried.
	Limit Limit
	// Charged is what the space has already paid for this content, which is
	// the declared size of an upload session of spec 007. The transaction
	// charges the difference, so a session that opened and completed is
	// charged once.
	Charged int64
	// Also runs inside the write's transaction, after the row is written and
	// the space charged. It is how the session row of spec 007 leaves in the
	// commit the object it assembled arrives in.
	Also func(ctx context.Context, q store.Querier) error
}

// Result is what a committed Write left behind.
type Result struct {
	// File is the row as it now stands.
	File store.File
	// Created reports that no live row held the path before the write,
	// which is the 201 of spec 013 and takes a revived trashed path with
	// it: a path nobody could read was a path that was not there.
	Created bool
	// Replaced is the row this write superseded, zero when the path had
	// none.
	Replaced store.File
	// Versioned reports that Replaced was captured as a version, so its
	// bytes are still referenced and no caller may delete them.
	Versioned bool
}

// Commit writes the row of a put or of a completed upload session in one
// transaction, and is taken by both so the two writes cannot drift apart in
// their conditional behaviour, their version capture or their charge.
//
// In order: hold the row, read the precondition against what the lock
// answered, capture the version an overwrite supersedes, apply the write
// with the precondition repeated in SQL, and charge the ledger. Holding the
// row first is what stops two writers of one path from reading one version
// number; repeating the precondition in SQL is what stops two writers that
// both passed a pre-read from both committing.
//
// A refused precondition and a charge past the answer's limit both roll the
// transaction back, so the caller's fresh key is left with no row pointing
// at it and the caller deletes it at once.
func (s *Service) Commit(ctx context.Context, w Write) (Result, error) {
	var out Result
	err := s.db.Tx(ctx, func(q store.Querier) error {
		current, err := s.files.GetForUpdate(ctx, q, w.Owner, w.Path)
		switch {
		case err == nil:
			out.Replaced = current
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return fault(ctx, "read the path", err)
		}
		held := current.ID != ""
		live := held && current.DeletedAt == nil
		out.Created = !live

		// A trashed row holds no readable content, so a precondition is
		// read against the live checksum and against nothing otherwise:
		// If-Match on a trashed path names a state no reader could have
		// seen, and If-None-Match: * finds nothing and revives it.
		stored := ""
		if live {
			stored = current.Checksum
		}
		if !w.Pre.Allows(stored) {
			return refusePrecondition(w.Pre, stored)
		}

		// Only the files plane keeps versions. The workspaces plane is the
		// sync protocol's, which would read a version as a file it did not
		// write (spec 009).
		out.Versioned = w.Plane == object.PlaneFiles && held
		if out.Versioned {
			if _, err := s.versions.Capture(ctx, q, w.Owner, w.Path, ifMatchOf(w.Pre)); err != nil {
				return fault(ctx, "capture the version", err)
			}
		}

		row := store.File{
			Owner: w.Owner, Path: w.Path, ObjectID: w.Object, CreatedBy: w.Actor,
			ContentType: w.ContentType, SizeBytes: w.Size,
			Checksum: w.Checksum, ChecksumKind: w.ChecksumKind,
		}
		if err := s.apply(ctx, q, row, w.Pre, stored); err != nil {
			return err
		}

		// A versioned overwrite grows the space by the whole new size,
		// because the replaced object survives as a version. A write that
		// keeps no second copy nets out what it replaced.
		replaced := int64(0)
		if held && !out.Versioned {
			replaced = current.SizeBytes
		}
		if _, err := s.charge(ctx, q, w.Owner, w.Size-replaced-w.Charged, w.Limit); err != nil {
			return err
		}
		if w.Also != nil {
			if err := w.Also(ctx, q); err != nil {
				return err
			}
		}

		written, err := s.files.Get(ctx, q, w.Owner, w.Path)
		if err != nil {
			return fault(ctx, "read the path back", err)
		}
		out.File = written
		return nil
	})
	if err != nil {
		return Result{}, Committed(ctx, err)
	}
	return out, nil
}

// apply is the arm of the write the precondition selects. The condition is
// in the statement in two of the three, and the third holds the row.
func (s *Service) apply(ctx context.Context, q store.Querier, row store.File, pre api.Precondition, stored string) error {
	switch {
	case pre.CreateOnly:
		created, err := s.files.CreateOnly(ctx, q, row)
		if err != nil {
			return fault(ctx, "write the path", err)
		}
		if !created {
			return refusePrecondition(pre, stored)
		}
	case len(pre.IfMatch) > 0:
		applied, err := s.files.UpdateIfChecksum(ctx, q, row, stored)
		if err != nil {
			return fault(ctx, "write the path", err)
		}
		if !applied {
			return refusePrecondition(pre, stored)
		}
	default:
		if err := s.files.Upsert(ctx, q, row); err != nil {
			return fault(ctx, "write the path", err)
		}
	}
	return nil
}

// charge applies a delta to the ledger inside the write's own transaction
// and renders a refusal the caller reads as a space with no room left.
func (s *Service) charge(ctx context.Context, q store.Querier, owner string, delta int64, limit Limit) (int64, error) {
	total, err := s.ledger.Charge(ctx, q, owner, delta, limit)
	if err == nil {
		return total, nil
	}
	var over *OverLimit
	if errors.As(err, &over) {
		// Arca stores no limit, so the count of refusals is the only thing
		// about one it can publish (spec 018).
		s.metrics.LimitRejected()
		return 0, api.Refuse(api.CodeQuotaExceeded, "%s", over.Error())
	}
	return 0, fault(ctx, "record what the space holds", err)
}

// ifMatchOf is the checksum a capture is conditioned on, so a capture that
// lost the race writes no history. A write with no If-Match captures
// unconditionally, because it holds the row.
func ifMatchOf(pre api.Precondition) string {
	if len(pre.IfMatch) == 1 {
		return pre.IfMatch[0]
	}
	return ""
}

// refusePrecondition says which header refused the write and what the object
// holds, which is the one thing a caller needs to retry.
func refusePrecondition(pre api.Precondition, stored string) error {
	switch {
	case pre.CreateOnly:
		return api.Refuse(api.CodePreconditionFailed,
			"If-None-Match asked for a path nothing holds, and this one holds %s", quoted(stored))
	case len(pre.IfMatch) > 0:
		return api.Refuse(api.CodePreconditionFailed,
			"If-Match named %v; the path holds %s", pre.IfMatch, quoted(stored))
	default:
		return api.Refuse(api.CodePreconditionFailed,
			"If-None-Match named the checksum the path holds, %s", quoted(stored))
	}
}

// quoted renders a checksum for the developer detail, with a word for the
// path that holds none.
func quoted(checksum string) string {
	if checksum == "" {
		return "nothing"
	}
	return checksum
}
