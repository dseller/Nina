package oas

import (
	"fmt"
	"strings"

	yaml "go.yaml.in/yaml/v4"
)

// This file holds the yaml.Node primitives the merger is built on. We work on the
// raw node tree rather than libopenapi's typed model because the merge has to be
// lossless: vendor extensions, examples, descriptions and key ordering all have to
// survive into the published document, and every $ref has to be rewritten under our
// control. See libopenapi_contract_test.go for the properties we rely on.

func docRoot(n *yaml.Node) *yaml.Node {
	if n != nil && n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return n.Content[0]
	}
	return n
}

func newMap() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

func newSeq() *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
}

func newScalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

// mapGet returns the value node for key, or nil.
func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mapSet replaces the value for key, appending the pair if absent.
func mapSet(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, newScalar(key), val)
}

// mapGetOrCreate returns the mapping at key, creating it if missing.
func mapGetOrCreate(m *yaml.Node, key string) *yaml.Node {
	if v := mapGet(m, key); v != nil {
		return v
	}
	v := newMap()
	mapSet(m, key, v)
	return v
}

// mapKeys returns the mapping's keys in document order.
func mapKeys(m *yaml.Node) []string {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]string, 0, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		out = append(out, m.Content[i].Value)
	}
	return out
}

// deepCopy clones a node tree. Aliases are not followed; the yaml decoder has
// already expanded them by the time we see the tree.
func deepCopy(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	c := *n
	c.Content = nil
	if len(n.Content) > 0 {
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, ch := range n.Content {
			c.Content[i] = deepCopy(ch)
		}
	}
	return &c
}

// walkRefs calls fn for every "$ref" scalar value node in the tree.
//
// A mapping is treated as a $ref holder only when "$ref" appears as a key; the
// walk still descends into the sibling values, because OAS 3.1 permits $ref
// alongside other keywords.
func walkRefs(n *yaml.Node, fn func(ref *yaml.Node)) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value == "$ref" && v.Kind == yaml.ScalarNode {
				fn(v)
				continue
			}
			walkRefs(v, fn)
		}
		return
	}
	for _, c := range n.Content {
		walkRefs(c, fn)
	}
}

// parseLocalRef splits "#/components/schemas/User" into ("schemas", "User", "").
// A pointer into the middle of a component, such as
// "#/components/schemas/User/properties/id", yields tail "properties/id": we
// import the component whole, so only the head decides what to copy, but the
// tail has to survive the rewrite.
//
// ok is false for anything that is not a local components pointer, which after
// bundling should not occur.
func parseLocalRef(ref string) (section, name, tail string, ok bool) {
	const prefix = "#/components/"
	if !strings.HasPrefix(ref, prefix) {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(ref, prefix)
	i := strings.IndexByte(rest, '/')
	if i <= 0 || i == len(rest)-1 {
		return "", "", "", false
	}
	section, rest = rest[:i], rest[i+1:]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		name, tail = rest[:j], rest[j+1:]
	} else {
		name = rest
	}
	if name == "" {
		return "", "", "", false
	}
	return section, unescapePointer(name), tail, true
}

// unescapePointer applies RFC 6901 token unescaping.
func unescapePointer(s string) string {
	s = strings.ReplaceAll(s, "~1", "/")
	return strings.ReplaceAll(s, "~0", "~")
}

// escapePointer applies RFC 6901 token escaping.
func escapePointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}

func componentRef(section, name string) string {
	return fmt.Sprintf("#/components/%s/%s", section, escapePointer(name))
}
