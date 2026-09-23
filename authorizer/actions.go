// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package authorizer

import (
	"slices"

	"latere.ai/x/pkg/authz"
)

// Core is the name the shared contract carries this vocabulary under, which
// an endpoint's refusal of an unknown action names and which the grants on a
// personal key qualify their actions with.
const Core = "arca"

// The seven resource kinds of the arca.latere.ai/v1 API group. They are
// strings because the vocabulary is the half of the contract an endpoint
// needs, and the kind an action acts on never changes.
const (
	KindFile      = "File"
	KindUpload    = "Upload"
	KindShare     = "Share"
	KindLink      = "Link"
	KindWorkspace = "Workspace"
	KindEvent     = "Event"
	KindSpace     = "Space"
)

// The actions of spec 006's table: every question arcad asks an authorizer,
// and with the resource kinds above the whole of what this module adds to
// the shared contract's envelope. A constant never changes its string and
// never disappears; a new action is a new row in that table first and a
// constant here second.
const (
	ActionFileRead    = "file.read"
	ActionFileWrite   = "file.write"
	ActionFileDelete  = "file.delete"
	ActionFileList    = "file.list"
	ActionFileRestore = "file.restore"

	ActionUploadWrite = "upload.write"

	ActionShareCreate = "share.create"
	ActionShareRead   = "share.read"
	ActionShareList   = "share.list"
	ActionShareRevoke = "share.revoke"

	ActionLinkCreate = "link.create"
	ActionLinkRead   = "link.read"
	ActionLinkRevoke = "link.revoke"

	ActionWorkspaceCreate  = "workspace.create"
	ActionWorkspaceRead    = "workspace.read"
	ActionWorkspaceWrite   = "workspace.write"
	ActionWorkspaceDelete  = "workspace.delete"
	ActionWorkspaceList    = "workspace.list"
	ActionWorkspaceAttach  = "workspace.attach"
	ActionWorkspaceSync    = "workspace.sync"
	ActionWorkspaceRestore = "workspace.restore"

	ActionEventRead = "event.read"

	ActionSpaceAdmin = "space.admin"
)

// table is spec 006's action table in its order, one row per action, and the
// one place the pairing of an action with its kind is written. An action acts
// on exactly one kind, so every row's kind is its prefix capitalized.
var table = []authz.Action{
	{Name: ActionFileRead, Kind: KindFile},
	{Name: ActionFileWrite, Kind: KindFile},
	{Name: ActionFileDelete, Kind: KindFile},
	{Name: ActionFileList, Kind: KindFile},
	{Name: ActionFileRestore, Kind: KindFile},

	{Name: ActionUploadWrite, Kind: KindUpload},

	{Name: ActionShareCreate, Kind: KindShare},
	{Name: ActionShareRead, Kind: KindShare},
	{Name: ActionShareList, Kind: KindShare},
	{Name: ActionShareRevoke, Kind: KindShare},

	{Name: ActionLinkCreate, Kind: KindLink},
	{Name: ActionLinkRead, Kind: KindLink},
	{Name: ActionLinkRevoke, Kind: KindLink},

	{Name: ActionWorkspaceCreate, Kind: KindWorkspace},
	{Name: ActionWorkspaceRead, Kind: KindWorkspace},
	{Name: ActionWorkspaceWrite, Kind: KindWorkspace},
	{Name: ActionWorkspaceDelete, Kind: KindWorkspace},
	{Name: ActionWorkspaceList, Kind: KindWorkspace},
	{Name: ActionWorkspaceAttach, Kind: KindWorkspace},
	{Name: ActionWorkspaceSync, Kind: KindWorkspace},
	{Name: ActionWorkspaceRestore, Kind: KindWorkspace},

	{Name: ActionEventRead, Kind: KindEvent},

	{Name: ActionSpaceAdmin, Kind: KindSpace},
}

// labels is the name a person reads for each resource kind. A kind is a type
// name, and a person choosing what a personal key may do picks a function
// under a heading: the picker groups by kind, because an action acts on
// exactly one kind and that grouping is the only correct one, and it names
// each group with the label here rather than with a word of its own.
var labels = map[string]string{
	KindFile:      "Files",
	KindUpload:    "Uploads",
	KindShare:     "Shares",
	KindLink:      "Links",
	KindWorkspace: "Workspaces",
	KindEvent:     "Events",
	KindSpace:     "Spaces",
}

// Vocabulary is Arca's action table as the shared contract reads it: the
// client refuses an action outside it before the wire, the endpoint scaffold
// of latere.ai/x/pkg/authz/server answers a 400 for one, the conformance
// suite drives a case per row, and a picker reads the heading of each kind
// off Label. The value is a fresh copy each call, so a caller that sorts or
// appends to it, or declares labels of its own, changes nothing here.
func Vocabulary() authz.Vocabulary {
	return authz.Vocabulary{Core: Core, Actions: slices.Clone(table)}.WithLabels(labels)
}

// Actions lists every action of the vocabulary, in the table's order.
func Actions() []string {
	out := make([]string, len(table))
	for i, a := range table {
		out[i] = a.Name
	}
	return out
}

// Kind is the resource kind an action acts on, and "" for a string outside
// the vocabulary.
func Kind(action string) string {
	for _, a := range table {
		if a.Name == action {
			return a.Kind
		}
	}
	return ""
}

// Known reports whether action is one of the vocabulary.
func Known(action string) bool { return Kind(action) != "" }
