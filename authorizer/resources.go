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
// Grant, Grants and GrantsTruncated are the fields no handler sets. A file
// and an upload carry the rung the caller holds on the resource's own
// subtree and every subject's rung there, which internal/auth resolves from
// Arca's grants table before it asks; the four kinds that carry none are
// the ones no grant reaches (spec 006).

// Bytes is a size an object carries, for the Size field of a question that
// knows one. It exists so a zero-byte object sends a zero and a question
// that has not looked the object up sends nothing at all.
func Bytes(n int64) *int64 { return new(n) }

// MaxGrants is the most entries the grants member of a question lists. A
// question about a path more subjects hold a grant on lists the first
// MaxGrants of them by subject and carries grants_truncated.
const MaxGrants = 64

// MaxGrantsBytes bounds the bytes of the subjects and rungs the grants
// member lists, beside MaxGrants: the list stops before the entry that
// would pass it and carries grants_truncated. With MaxGranteeBytes it keeps
// the question, which an endpoint reads whole before it decides, far under
// the 64 KiB an endpoint reads, whatever a space has shared.
const MaxGrantsBytes = 16 << 10

// MaxGranteeBytes bounds the grantee a share names: a rendered subject
// "<issuer>|<sub>" is an issuer's URL and an identifier, each well under
// half of it.
const MaxGranteeBytes = 512

// Holder is one subject's rung on a resource, an entry of the grants member
// of [File] and [Upload].
type Holder struct {
	// Subject is the grantee a share names, the rendered subject
	// "<issuer>|<sub>" of a person, a service or an organization.
	Subject string
	// Permission is the highest rung the subject holds on a prefix of the
	// resource's path: read, write, or manage.
	Permission string
}

// Holders renders holders as the grants member carries them: one object
// {"subject", "permission"} per holder, in the order given. Nil renders as
// nil, which a resource leaves out; an empty list renders as an empty one,
// which a resource sends.
func Holders(list []Holder) []any {
	if list == nil {
		return nil
	}
	out := make([]any, 0, len(list))
	for _, h := range list {
		out = append(out, map[string]any{"subject": h.Subject, "permission": h.Permission})
	}
	return out
}

// File is the resource of every file.* action: a put, a read, a head, a
// listing, a move, a copy, a delete, a version, a star, and a restore from
// trash. A write that creates carries no id, because the id does not exist
// until the write is allowed.
type File struct {
	// ID is the object's id, absent on a write that creates and on a
	// question about a folder.
	ID string
	// Owner is the space the object lives in.
	Owner string
	// Path is the object's path within the space, and on a move or a copy
	// the target.
	Path string
	// Plane is "files" and nothing else (spec 001). The member stays on the
	// wire so an endpoint that reads it keeps answering.
	Plane string
	// Size is the object's size in bytes, absent where the question carries
	// no size: a listing, a folder delete, or a read of an object not yet
	// looked up. On a copy it is the sum of the bytes to be written, and on
	// a folder move the sum of the bytes under the folder.
	Size *int64
	// From is the source path of a move or a copy, absent on every other
	// question.
	From string
	// Recursive reports that Path, and From where there is one, name a
	// folder and everything under it: a folder delete, a folder copy or a
	// folder move. It is absent unless true.
	Recursive bool
	// Copy reports that From is read and stays where it is. A question that
	// carries From without it is a move. It is absent unless true.
	Copy bool
	// Grant is the rung the caller holds on a prefix of Path in Owner's
	// space: read, write, or manage, absent where the caller holds none. On
	// a question that names From it is the lower of the rungs held on the
	// two paths. internal/auth reads it off Arca's own grants table and puts
	// it here before the question goes out, so a caller building a File
	// leaves it empty.
	Grant string
	// Grants is every subject holding a live grant on a prefix of Path in
	// Owner's space, each with its highest rung there, ordered by subject,
	// at most [MaxGrants] of them. On a question that names From a subject
	// is listed only when it holds a rung on both paths, with the lower of
	// the two, as Grant is read. It is sent, empty where nobody holds one,
	// on every question that carries Grant's conditions: a path, a caller,
	// and a space that is not the caller's own. It is absent otherwise, and
	// from a server that predates it.
	//
	// Grant is the caller's own rung, and Grants is everyone's. An endpoint
	// that decides a question as a subject other than the caller, such as
	// an agent's question decided as the person it acts for, reads that
	// subject's rung here: Arca reads no claim, so it cannot know whom the
	// endpoint decides as. internal/auth fills it on the same terms as
	// Grant, so a caller building a File leaves it nil.
	Grants []Holder
	// GrantsTruncated reports that more subjects hold a grant than Grants
	// lists. A subject missing from a truncated list may hold a rung, so an
	// endpoint that decides as a subject it does not find there refuses
	// rather than read it as none. It is absent unless true.
	GrantsTruncated bool
}

// Resource renders the file as the envelope carries it.
func (f File) Resource() authz.Resource {
	return authz.NewResource(KindFile, f.ID, fields(
		field{"owner", f.Owner}, field{"path", f.Path}, field{"plane", f.Plane},
		size(f.Size), field{"from", f.From},
		set("recursive", f.Recursive), set("copy", f.Copy),
		field{"grant", f.Grant}, holders(f.Grants), set("grants_truncated", f.GrantsTruncated),
	))
}

// Upload is the resource of upload.write: the session that opens, completes,
// or aborts a multipart write. The object does not exist yet, so there is no
// id, and the size is the one the session declares.
type Upload struct {
	Owner string
	Path  string
	Size  *int64
	// Grant is the rung the caller holds on a prefix of Path, filled and
	// left empty on the same terms as a file's. upload.write is on the
	// write rung's ladder, so a grantee writing an object too large to
	// inline asks this question and not [File]'s; without the field the
	// ladder would name an action no question could ever satisfy.
	Grant string
	// Grants and GrantsTruncated are every subject's rung on a prefix of
	// Path, filled and left empty on the same terms as a file's.
	Grants          []Holder
	GrantsTruncated bool
}

// Resource renders the upload as the envelope carries it.
func (u Upload) Resource() authz.Resource {
	return authz.NewResource(KindUpload, "", fields(
		field{"owner", u.Owner}, field{"path", u.Path}, size(u.Size),
		field{"grant", u.Grant}, holders(u.Grants), set("grants_truncated", u.GrantsTruncated),
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

// holders is the grants member, left out when the list is nil and sent,
// as an empty array included, when it is not: an empty list says nobody
// holds a grant, and an absent one that the question does not say.
func holders(list []Holder) field {
	if list == nil {
		return field{}
	}
	return field{"grants", Holders(list)}
}

// set is a member that is sent as true or not sent at all, so an endpoint
// that predates it reads the question it always read.
func set(name string, on bool) field {
	if !on {
		return field{}
	}
	return field{name, true}
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
