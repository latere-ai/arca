// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package deploy

import (
	"fmt"
	"strconv"
	"strings"
)

// The reader below turns a manifest into a tree so the tests can assert on
// structure rather than on substrings. It reads the subset of YAML the
// deploy tree is written in and refuses everything else, which is what
// keeps it honest: a manifest that used an anchor, a merge key, a block
// scalar or a flow mapping would be read wrongly by a lenient reader and
// silently pass a check it should fail, so this one stops instead.
//
// A full parser is not vendored because the gate's hermetic run allows only
// the Go toolchain and the module takes no test-only dependency for this;
// the kustomize render itself, which needs kustomize, runs in CI where it
// is on PATH.

// node is one mapping entry, one sequence entry, or one scalar. A mapping
// entry has a key; a sequence entry does not. A node with children carries
// no value.
type node struct {
	key      string
	value    string
	children []*node
	line     int
}

// child returns the mapping child with this key, or nil.
func (n *node) child(key string) *node {
	if n == nil {
		return nil
	}
	for _, c := range n.children {
		if c.key == key {
			return c
		}
	}
	return nil
}

// at walks a path of mapping keys and returns the node it ends on, or nil.
func (n *node) at(path ...string) *node {
	cur := n
	for _, key := range path {
		cur = cur.child(key)
		if cur == nil {
			return nil
		}
	}
	return cur
}

// text returns the scalar at a path, or "" when the path is absent or holds
// children rather than a value.
func (n *node) text(path ...string) string {
	got := n.at(path...)
	if got == nil {
		return ""
	}
	return got.value
}

// items returns the sequence entries at a path. A path that is absent, or
// holds a scalar, yields none.
func (n *node) items(path ...string) []*node {
	got := n.at(path...)
	if got == nil {
		return nil
	}
	var out []*node
	for _, c := range got.children {
		if c.key == "" {
			out = append(out, c)
		}
	}
	return out
}

// strings returns the sequence entries at a path as scalars, dropping the
// entries that are mappings.
func (n *node) strings(path ...string) []string {
	var out []string
	for _, item := range n.items(path...) {
		if item.value != "" {
			out = append(out, item.value)
		}
	}
	return out
}

// parse reads every document of one manifest. An error names the line, so a
// manifest this reader cannot read is reported rather than passed over.
func parse(text string) ([]*node, error) {
	var docs []*node
	root := &node{}
	stack := []*node{root}
	indents := []int{-1}
	flush := func() {
		if len(root.children) > 0 {
			docs = append(docs, root)
		}
		root = &node{}
		stack = []*node{root}
		indents = []int{-1}
	}
	for i, raw := range strings.Split(text, "\n") {
		line := i + 1
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		if strings.TrimSpace(raw) == "---" {
			flush()
			continue
		}
		if strings.Contains(raw, "\t") {
			return nil, fmt.Errorf("line %d: a tab indents this line; the tree is written with spaces", line)
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		content := strings.TrimSpace(raw)
		// A sequence entry opens a node of its own, and its inline
		// remainder is read as a line two columns further in, which is
		// where a second key of the same entry is written.
		var item *node
		if strings.HasPrefix(content, "- ") || content == "-" {
			item = &node{line: line}
			if err := attach(&stack, &indents, indent, item); err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			stack = append(stack, item)
			indents = append(indents, indent+1)
			indent += 2
			content = strings.TrimSpace(strings.TrimPrefix(content, "-"))
			if content == "" {
				continue
			}
		}
		key, value, err := entry(content)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		// A sequence entry that is one scalar is that scalar, not a
		// mapping holding it.
		if key == "" && item != nil {
			item.value = value
			continue
		}
		n := &node{key: key, value: value, line: line}
		if flow, ok := strings.CutPrefix(value, "["); ok {
			rest, closed := strings.CutSuffix(flow, "]")
			if !closed {
				return nil, fmt.Errorf("line %d: a flow sequence that does not close on its line", line)
			}
			n.value = ""
			for part := range strings.SplitSeq(rest, ",") {
				if part = strings.TrimSpace(part); part != "" {
					n.children = append(n.children, &node{value: unquote(part), line: line})
				}
			}
			if rest = strings.TrimSpace(rest); rest != "" && len(n.children) == 0 {
				return nil, fmt.Errorf("line %d: a flow sequence this reader cannot read", line)
			}
		}
		if err := attach(&stack, &indents, indent, n); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if n.value == "" && len(n.children) == 0 {
			stack = append(stack, n)
			indents = append(indents, indent)
		}
	}
	flush()
	return docs, nil
}

// attach hangs a node under the deepest open parent for its indentation.
func attach(stack *[]*node, indents *[]int, indent int, n *node) error {
	for len(*indents) > 1 && indent <= (*indents)[len(*indents)-1] {
		*stack = (*stack)[:len(*stack)-1]
		*indents = (*indents)[:len(*indents)-1]
	}
	parent := (*stack)[len(*stack)-1]
	if parent.value != "" {
		return fmt.Errorf("this line is indented under a scalar")
	}
	parent.children = append(parent.children, n)
	return nil
}

// entry splits one mapping line into its key and its scalar. A line with no
// colon is a scalar sequence entry.
func entry(content string) (key, value string, err error) {
	if strings.HasPrefix(content, `"`) || strings.HasPrefix(content, "'") {
		return "", unquote(content), nil
	}
	before, after, cut := strings.Cut(content, ": ")
	switch {
	case cut:
		key, value = before, strings.TrimSpace(after)
	case strings.HasSuffix(content, ":"):
		key = strings.TrimSuffix(content, ":")
	default:
		return "", unquote(content), nil
	}
	if key == "<<" || strings.ContainsAny(key, "{}&*|>") {
		return "", "", fmt.Errorf("the key %q uses a construct this reader does not read", key)
	}
	// The empty mapping is the one flow mapping the tree is written with,
	// for a volume that is an emptyDir and a list of alert groups that is
	// still empty. It reads as a mapping with nothing in it, which is what
	// it is.
	if value == "{}" {
		return key, "", nil
	}
	// An anchor, an alias, a flow mapping, and a block scalar each mean
	// something this reader would otherwise read as a plain string.
	if strings.HasPrefix(value, "{") || strings.HasPrefix(value, "&") ||
		strings.HasPrefix(value, "*") || value == "|" || value == ">" ||
		value == "|-" || value == ">-" {
		return "", "", fmt.Errorf("the value of %q uses a construct this reader does not read", key)
	}
	return key, unquote(value), nil
}

// unquote removes one layer of quoting. A value this reader leaves quoted
// would never equal the value a test compares it with.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		if s[0] == '"' {
			if out, err := strconv.Unquote(s); err == nil {
				return out
			}
		}
		return s[1 : len(s)-1]
	}
	return s
}
