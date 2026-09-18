// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package authorizer is the vocabulary an authorization endpoint for Arca
// is written against: the actions arcad asks, the resource kind each one
// acts on, and the shape of the resource each question carries. Import it
// to write the endpoint ARCA_AUTHORIZER_URL points at, in Go, instead of
// keeping a copy of the strings.
//
// The envelope on the wire is latere.ai/x/pkg/authz's, and this package
// declares none of it: arcad POSTs a request with the caller's subject,
// its claims verbatim, an action and a resource, and reads back an allow
// or a deny, all of it that package's types. This package is the Arca half
// of that contract and nothing more. Vocabulary is the whole action table
// as authz.Vocabulary, which the client refuses an unknown action against,
// latere.ai/x/pkg/authz/server validates against, and
// latere.ai/x/pkg/authz/conformance drives its cases from:
//
//	conformance.Run(t, url, token, conformance.WithVocabulary(authorizer.Vocabulary()))
//
// Actions lists every action, twenty-three of them over seven kinds, each
// one a constant here; Kind reports the kind an action acts on, and Known
// whether a string is in the vocabulary at all. Nothing here dials: the
// package builds values and decodes them, and the client is the caller's.
//
// The resource shapes are types of this package, one per kind, each with a
// Resource method that renders what the envelope carries. An endpoint reads
// a field off the flat object, resource.owner and not resource.fields.owner,
// so the types are a writer's convenience on both sides of the wire rather
// than a second declaration of it. A field the server does not know for a
// question is left out rather than sent empty, so an endpoint can tell "no
// source path" from "a source path this version does not send".
//
// A decision names a subject as the issuer and the sub joined,
// "https://issuer.example|9ab3", which is also how every owner field of
// the API renders a space. There is no other spelling of a principal.
//
// The promise, as for every package at this module's root: additive within
// a module major, and the same on every build. An action string never
// changes and never disappears, a resource kind stays the kind it is, and a
// resource field keeps its wire name and its meaning. A new action is a new
// row in spec 006's table first and a constant here second, so an endpoint
// that decides by the constants keeps compiling and an endpoint that decides
// by a default keeps deciding.
package authorizer
