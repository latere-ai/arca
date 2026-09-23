// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package apidocs builds the OpenAPI description of Arca's HTTP surface from
// the code, so the published document cannot silently drift from what the
// server serves.
//
// Two tables are the whole of what it reads, and it declares neither: the
// route table of internal/api, one row per registration with the authorizer
// action that row asks, and the error table of the same package, one row per
// code of spec 013. Both are handed in as data, which is what keeps this
// package free of the API's own imports and lets the API serve the document
// it describes.
//
// The document is Go values, so it renders to JSON with the standard library
// and needs no schema library on the server's build list (spec 001's ninth
// invariant). GET /openapi.json answers that JSON. tools/apidoc renders the
// same document to the YAML committed at api/openapi.yaml, and a test fails
// when the committed file is not what a fresh build produces, so a route
// added without regenerating does not reach main.
//
// The predecessor read its route set out of the source with a regular
// expression over mux.HandleFunc. Here the route table is a declaration the
// mux is built from, so the document and the registrations cannot disagree:
// there is one table and no second reading of it.
package apidocs

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
)

// Version is the OpenAPI version the document declares.
const Version = "3.1.0"

// Route is one endpoint of the surface as internal/api registers it: the
// method and path of the registration, the authorizer action the route asks,
// and whether it sits outside the verifier.
type Route struct {
	Method string
	// Path is the registration's path, with its wildcards in the router's
	// own spelling: "/v1/files/{owner}/{path...}".
	Path string
	// Action is the action of spec 006's vocabulary this route asks before
	// it acts, and "" for a route that asks nothing. Only the three public
	// link routes of spec 013 ask nothing.
	Action string
	// Public reports a route registered outside the verifier, which carries
	// no bearer token.
	Public bool
	// Summary is a verb-first navigation label of at most four words.
	Summary string
	// Description preserves behavior, qualifications, and alternatives.
	Description string
	// Status is the status a success answers.
	Status int
	// Pending reports a route the frame registers and does not yet answer,
	// whose behavior belongs to a spec that has not landed. It answers
	// not_implemented, and the document says so rather than describing a
	// body nothing returns.
	Pending bool
}

// ErrorCode is one row of spec 013's error table.
type ErrorCode struct {
	Code     string
	Status   int
	Sentence string
}

// Options is everything the document is built from.
type Options struct {
	Title       string
	Version     string
	Description string
	// Server is the base a client reaches this installation at,
	// ARCA_PUBLIC_URL. Empty leaves the document without a servers block,
	// which is what the committed file carries: a document in a repository
	// describes no one installation.
	Server string
	Routes []Route
	Errors []ErrorCode
}

// Document is the OpenAPI description. Its fields are in the order a reader
// expects them, and every map the standard library renders is sorted by key,
// so two builds of one table are byte for byte the same file.
type Document struct {
	OpenAPI    string              `json:"openapi"`
	Info       Info                `json:"info"`
	Servers    []Server            `json:"servers,omitempty"`
	Paths      map[string]PathItem `json:"paths"`
	Components Components          `json:"components"`
}

// Info is the document's own identity.
type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// Server is one base URL the surface is served at.
type Server struct {
	URL string `json:"url"`
}

// PathItem is the operations of one path, keyed by the lower-case method.
type PathItem map[string]Operation

// Operation is one route.
type Operation struct {
	OperationID string              `json:"operationId"`
	Summary     string              `json:"summary,omitempty"`
	Description string              `json:"description,omitempty"`
	Parameters  []Parameter         `json:"parameters,omitempty"`
	Security    []map[string][]any  `json:"security"`
	Responses   map[string]Response `json:"responses"`
}

// Parameter is one wildcard of a path.
type Parameter struct {
	Name        string `json:"name"`
	In          string `json:"in"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
	Schema      Schema `json:"schema"`
}

// Response is one answer, or a reference to one the components declare.
type Response struct {
	Ref         string               `json:"$ref,omitempty"`
	Description string               `json:"description,omitempty"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

// MediaType is one content type of a response.
type MediaType struct {
	Schema Schema `json:"schema"`
}

// Schema is a JSON Schema, as much of one as this document needs.
type Schema struct {
	Ref                  string            `json:"$ref,omitempty"`
	Type                 string            `json:"type,omitempty"`
	Description          string            `json:"description,omitempty"`
	Properties           map[string]Schema `json:"properties,omitempty"`
	Required             []string          `json:"required,omitempty"`
	Items                *Schema           `json:"items,omitempty"`
	AdditionalProperties *bool             `json:"additionalProperties,omitempty"`
}

// Components are the pieces the operations reference: the bearer scheme, the
// error envelope, and one response per code of spec 013's table.
type Components struct {
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes"`
	Schemas         map[string]Schema         `json:"schemas"`
	Responses       map[string]Response       `json:"responses"`
}

// SecurityScheme is the one credential the surface takes.
type SecurityScheme struct {
	Type         string `json:"type"`
	Scheme       string `json:"scheme"`
	BearerFormat string `json:"bearerFormat,omitempty"`
	Description  string `json:"description,omitempty"`
}

// bearer is the name the security scheme is referenced by.
const bearer = "bearer"

// Build renders the document from the two tables.
func Build(o Options) *Document {
	d := &Document{
		OpenAPI: Version,
		Info:    Info{Title: o.Title, Version: o.Version, Description: o.Description},
		Paths:   map[string]PathItem{},
		Components: Components{
			SecuritySchemes: map[string]SecurityScheme{bearer: {
				Type: "http", Scheme: "bearer", BearerFormat: "JWT",
				Description: "A token from an issuer this installation lists, carrying the audience it verifies.",
			}},
			Schemas:   schemas(),
			Responses: responses(o.Errors),
		},
	}
	if o.Server != "" {
		d.Servers = []Server{{URL: o.Server}}
	}
	byStatus := map[string][]ErrorCode{}
	for _, e := range o.Errors {
		key := fmt.Sprint(e.Status)
		byStatus[key] = append(byStatus[key], e)
	}
	for _, r := range o.Routes {
		path := template(r.Path)
		item, ok := d.Paths[path]
		if !ok {
			item = PathItem{}
			d.Paths[path] = item
		}
		item[strings.ToLower(r.Method)] = operation(r, byStatus)
	}
	return d
}

// template renders a registration's path as OpenAPI writes one. The router
// spells a segment that swallows the rest of the path "{path...}", and a
// path template has no such form: the parameter is named once and its
// description says it carries slashes. Nothing else about a path changes.
func template(path string) string {
	return strings.ReplaceAll(path, "...}", "}")
}

// operation renders one route.
func operation(r Route, byStatus map[string][]ErrorCode) Operation {
	op := Operation{
		OperationID: operationID(r),
		Summary:     r.Summary,
		Description: describe(r),
		Parameters:  parameters(r.Path),
		Security:    []map[string][]any{},
		Responses:   map[string]Response{},
	}
	if !r.Public {
		op.Security = []map[string][]any{{bearer: {}}}
	}
	for _, code := range answered(r) {
		status := statusOf(code, byStatus)
		op.Responses[status] = answer(status, byStatus[status], code)
	}
	if !r.Pending && r.Status != 0 {
		op.Responses[fmt.Sprint(r.Status)] = Response{
			Description: http.StatusText(r.Status),
			Content:     map[string]MediaType{"application/json": {}},
		}
	}
	return op
}

// answered is the codes of spec 013's table a route can answer. Every route
// answers the frame's own: a rate the caller crossed and a server that
// failed. A route behind the verifier adds the refusals of spec 006. A route
// the frame registers and does not yet answer has the one code that says so
// and nothing else, because it reaches no handler that could answer more.
func answered(r Route) []string {
	codes := []string{"rate_limited", "internal"}
	if r.Pending {
		return append(codes, "not_implemented")
	}
	if !r.Public {
		codes = append(codes, "unauthenticated", "forbidden", "not_found", "authorizer_unavailable")
	}
	return codes
}

// statusOf is the status a code is answered with.
func statusOf(code string, byStatus map[string][]ErrorCode) string {
	for status, codes := range byStatus {
		if slices.ContainsFunc(codes, func(e ErrorCode) bool { return e.Code == code }) {
			return status
		}
	}
	return ""
}

// answer renders one status of an operation. A status with one code in the
// whole table is a reference to that code's declared response; a status
// several codes share carries a description naming the one this route
// answers, since OpenAPI keys an operation's answers by status and the codes
// are what a caller branches on.
func answer(status string, sharing []ErrorCode, code string) Response {
	if len(sharing) == 1 {
		return Response{Ref: "#/components/responses/" + code}
	}
	return Response{
		Description: fmt.Sprintf("%s: %s", code, sentenceOf(code, sharing)),
		Content:     map[string]MediaType{"application/json": {Schema: Schema{Ref: "#/components/schemas/Error"}}},
	}
}

// sentenceOf is one code's user sentence out of the rows sharing its status.
func sentenceOf(code string, sharing []ErrorCode) string {
	for _, e := range sharing {
		if e.Code == code {
			return e.Sentence
		}
	}
	return ""
}

// describe combines operation details with authorization and availability.
func describe(r Route) string {
	var parts []string
	if r.Description != "" {
		parts = append(parts, r.Description)
	}
	switch {
	case r.Action != "":
		parts = append(parts, "Asks the authorizer for "+r.Action+" before it acts.")
	case r.Public:
		parts = append(parts, "Carries no bearer token: the token in the URL is the whole of the authorization.")
	}
	if r.Pending {
		parts = append(parts, "This build does not serve this route yet and answers not_implemented.")
	}
	return strings.Join(parts, " ")
}

// operationID is a stable name for one route, derived from its method and
// path so a generated client's method names do not move when a summary is
// reworded.
func operationID(r Route) string {
	var out strings.Builder
	out.WriteString(strings.ToLower(r.Method))
	for seg := range strings.SplitSeq(strings.Trim(r.Path, "/"), "/") {
		if seg == "" {
			continue
		}
		if name, ok := wildcard(seg); ok {
			out.WriteString("By" + title(name))
			continue
		}
		out.WriteString(title(seg))
	}
	return out.String()
}

// parameters are the wildcards of a path, in the order they appear.
func parameters(path string) []Parameter {
	var out []Parameter
	for seg := range strings.SplitSeq(path, "/") {
		name, ok := wildcard(seg)
		if !ok {
			continue
		}
		p := Parameter{Name: name, In: "path", Required: true, Schema: Schema{Type: "string"}}
		if strings.HasSuffix(seg, "...}") {
			p.Description = "The rest of the path, slashes included."
		}
		out = append(out, p)
	}
	return out
}

// wildcard reads the name of a path segment that is one, "{owner}" and
// "{path...}" alike.
func wildcard(seg string) (string, bool) {
	if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
		return "", false
	}
	return strings.TrimSuffix(strings.Trim(seg, "{}"), "..."), true
}

// title upper-cases a segment's first letter and drops what an identifier
// cannot carry, so an operation id reads as one word per segment.
func title(s string) string {
	var b strings.Builder
	upper := true
	for _, r := range s {
		switch {
		case r == '-' || r == '_' || r == '.':
			upper = true
		case upper:
			b.WriteString(strings.ToUpper(string(r)))
			upper = false
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// responses declares one answer per code of the error table, so a generator
// emits one type per code and an operation references rather than repeats
// them.
func responses(errs []ErrorCode) map[string]Response {
	out := map[string]Response{}
	for _, e := range errs {
		out[e.Code] = Response{
			Description: fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Sentence),
			Content:     map[string]MediaType{"application/json": {Schema: Schema{Ref: "#/components/schemas/Error"}}},
		}
	}
	return out
}

// no is the false additionalProperties of an object that takes no field
// beyond the ones it declares.
var no = false

// schemas are the two shapes every consumer meets: the error envelope of
// spec 013 and the list envelope its pagination answers in. The request and
// response bodies of each route are the owning spec's and join the document
// with the handlers that return them.
func schemas() map[string]Schema {
	return map[string]Schema{
		"Error": {
			Type:        "object",
			Description: "The one error shape of this API. A caller branches on error.code and shows error.message.",
			Required:    []string{"error"},
			Properties: map[string]Schema{"error": {
				Type:     "object",
				Required: []string{"code", "message"},
				Properties: map[string]Schema{
					"code":    {Type: "string", Description: "The stable identifier of the row of the error table."},
					"message": {Type: "string", Description: "The one sentence of that row, fixed and shown to a person."},
					"details": {
						Type:        "object",
						Description: "What varies: always request_id, the developer sentence in detail, and the field paths at fault in fields.",
						Properties: map[string]Schema{
							"request_id": {Type: "string"},
							"detail":     {Type: "string"},
							"fields":     {Type: "array", Items: &Schema{Type: "string"}},
						},
					},
				},
			}},
		},
		"Page": {
			Type:                 "object",
			Description:          "The one shape every list route answers. next_cursor is present only when a further page exists.",
			Required:             []string{"entries"},
			AdditionalProperties: &no,
			Properties: map[string]Schema{
				"entries":     {Type: "array", Items: &Schema{Type: "object"}},
				"next_cursor": {Type: "string"},
			},
		},
	}
}

// JSON renders the document as the bytes GET /openapi.json answers and as
// the bytes tools/apidoc converts to the committed YAML. It is indented, so
// a person reading either reads the same shape.
//
// It cannot fail: a Document is a closed set of strings, numbers, booleans,
// and slices and maps of those, and every one of them is JSON.
func (d *Document) JSON() []byte { return MustJSON(d) }

// MustJSON renders a value as the indented JSON this package writes, and
// panics on a value that is not JSON at all: a channel, a function, or a
// cycle. That is a programming error rather than a runtime condition, the
// same reading as a handler naming an error code nobody wrote a row for, and
// a panic is what keeps a caller from writing out bytes it never checked.
func MustJSON(v any) []byte {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic("apidocs: the document does not marshal: " + err.Error())
	}
	return append(out, '\n')
}

// Paths lists every path the document describes, sorted, for a test.
func (d *Document) PathList() []string {
	return slices.Sorted(maps.Keys(d.Paths))
}

// Operations lists every "METHOD path" the document describes, sorted.
func (d *Document) Operations() []string {
	var out []string
	for path, item := range d.Paths {
		for method := range item {
			out = append(out, strings.ToUpper(method)+" "+path)
		}
	}
	sort.Strings(out)
	return out
}
