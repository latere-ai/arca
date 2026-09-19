// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package authorizer

import "latere.ai/x/pkg/authz"

// The resource shapes of spec 006's table, one type per kind. They render
// what arcad sends and what an endpoint reads, so neither side writes the
// field names out by hand. On the wire a resource is one flat object, kind
// and id beside the fields, which is the shared envelope's shape:
// resource.owner and not resource.fields.owner.
//
// A field the server does not know for a question is left out rather than
// sent empty, so an authorizer that reads one can tell "no source path" from
// "a source path this version does not send". Owner is the rendered subject
// of spec 006, "<issuer>|<sub>", and never a bare sub.
//
// Grant is the one field no handler sets. A file and a workspace carry the
// rung the caller holds on the resource's own subtree, which internal/auth
// resolves from Arca's grants table before it asks; the four kinds that
// carry none are the ones no grant reaches (spec 006).

// Bytes is a size an object carries, for the Size field of a question that
// knows one. It exists so a zero-byte object sends a zero and a question
// that has not looked the object up sends nothing at all.
func Bytes(n int64) *int64 { return new(n) }

// File is the resource of every file.* action: a put, a read, a head, a
// listing, a move, a delete, a version, a star, and a restore from trash.
// A write that creates carries no id, because the id does not exist until
// the write is allowed.
type File struct {
	// ID is the object's id, absent on a write that creates.
	ID string
	// Owner is the space the object lives in.
	Owner string
	// Path is the object's path within the space, and on a move the target.
	Path string
	// Plane is "files" or "workspaces" and nothing else (spec 001).
	Plane string
	// Size is the object's size in bytes, absent where the question carries
	// no size: a listing, or a read of an object not yet looked up.
	Size *int64
	// From is the source path of a move, absent on every other question.
	From string
	// Grant is the rung the caller holds on a prefix of Path in Owner's
	// space: read, write, or manage, absent where the caller holds none.
	// internal/auth reads it off Arca's own grants table and puts it here
	// before the question goes out, so a caller building a File leaves it
	// empty.
	Grant string
}

// Resource renders the file as the envelope carries it.
func (f File) Resource() authz.Resource {
	return authz.NewResource(KindFile, f.ID, fields(
		field{"owner", f.Owner}, field{"path", f.Path}, field{"plane", f.Plane},
		size(f.Size), field{"from", f.From}, field{"grant", f.Grant},
	))
}

// Upload is the resource of upload.write: the session that opens, completes,
// or aborts a multipart write. The object does not exist yet, so there is no
// id, and the size is the one the session declares.
type Upload struct {
	Owner string
	Path  string
	Size  *int64
}

// Resource renders the upload as the envelope carries it.
func (u Upload) Resource() authz.Resource {
	return authz.NewResource(KindUpload, "", fields(
		field{"owner", u.Owner}, field{"path", u.Path}, size(u.Size),
	))
}

// Share is the resource of every share.* action: a grant of a permission to
// a subject on a subtree of a space. A create carries no id.
type Share struct {
	ID string
	// Owner is the space whose subtree is granted.
	Owner string
	// Path is the subtree the grant covers.
	Path string
	// Grantee is the rendered subject the grant is for.
	Grantee string
	// Permission is the rung of the ladder: read, write, or manage.
	Permission string
}

// Resource renders the share as the envelope carries it.
func (s Share) Resource() authz.Resource {
	return authz.NewResource(KindShare, s.ID, fields(
		field{"owner", s.Owner}, field{"path", s.Path},
		field{"grantee", s.Grantee}, field{"permission", s.Permission},
	))
}

// Link is the resource of every link.* action: a token grant on a subtree,
// which anyone holding the token may read. A create carries no id, and a
// redemption carries the id the token resolved to, so an operator turns
// public reading off by denying link.read.
type Link struct {
	ID    string
	Owner string
	Path  string
}

// Resource renders the link as the envelope carries it.
func (l Link) Resource() authz.Resource {
	return authz.NewResource(KindLink, l.ID, fields(
		field{"owner", l.Owner}, field{"path", l.Path},
	))
}

// Workspace is the resource of every workspace.* action: a durable subtree a
// sandbox attaches to. A create carries no id.
type Workspace struct {
	ID    string
	Owner string
	Slug  string
	// Grant is the rung the caller holds on the workspace's own subtree,
	// workspaces/<slug>, which is the prefix a grant on a workspace covers
	// (spec 001). It is filled and left empty on the same terms as a file's.
	Grant string
}

// Resource renders the workspace as the envelope carries it.
func (w Workspace) Resource() authz.Resource {
	return authz.NewResource(KindWorkspace, w.ID, fields(
		field{"owner", w.Owner}, field{"slug", w.Slug}, field{"grant", w.Grant},
	))
}

// Event is the resource of event.read: a space's log. The log is the space's,
// so the owner is the whole of what the question names.
type Event struct {
	Owner string
}

// Resource renders the event log as the envelope carries it.
func (e Event) Resource() authz.Resource {
	return authz.NewResource(KindEvent, "", fields(field{"owner", e.Owner}))
}

// Space is the resource of space.admin, the administrator's action. Owner is
// absent on the overview across spaces, which names no one space.
type Space struct {
	Owner string
}

// Resource renders the space as the envelope carries it.
func (s Space) Resource() authz.Resource {
	return authz.NewResource(KindSpace, "", fields(field{"owner", s.Owner}))
}

// Probe is the resource every authorizer denies for every subject and every
// action. The id is the shared contract's, not a value of Arca's own: one id
// is what lets one check command read one answer from every endpoint of the
// family, and an endpoint that allows it is one that does not read the
// request (spec 006, spec 012).
func Probe() authz.Resource {
	return authz.NewResource(KindSpace, authz.ProbeID, nil)
}

// field is one member of a resource, left out when it is empty.
type field struct {
	name  string
	value any
}

// size is the size member, left out when the question carries none and sent
// as a number, zero included, when it does.
func size(n *int64) field {
	if n == nil {
		return field{}
	}
	return field{"size", *n}
}

// fields gathers the members a resource carries, dropping the empty ones.
func fields(list ...field) map[string]any {
	out := map[string]any{}
	for _, f := range list {
		switch v := f.value.(type) {
		case nil:
			continue
		case string:
			if v == "" {
				continue
			}
		}
		out[f.name] = f.value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
