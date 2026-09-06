package oas

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pb33f/libopenapi"
	"github.com/pb33f/libopenapi/bundler"
	"github.com/pb33f/libopenapi/datamodel"
	yaml "go.yaml.in/yaml/v4"
)

// httpMethods are the path-item keys that denote an operation, in the order the
// OpenAPI specification lists them.
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

func isMethod(k string) bool {
	for _, m := range httpMethods {
		if k == m {
			return true
		}
	}
	return false
}

// Spec is a parsed, bundled, 3.1-normalised OpenAPI document.
type Spec struct {
	// Version is the document's declared OpenAPI version, before normalisation.
	Version string
	Title   string
	// APIVersion is info.version.
	APIVersion string

	root *yaml.Node // the document's root mapping
	ops  []*Operation
}

// Operation is a single upstream operation, with a handle on the raw node so the
// merger can copy it losslessly.
type Operation struct {
	Path        string // upstream path template, e.g. /users/{id}
	Method      string // upper-case, e.g. GET
	OperationID string
	Summary     string
	Deprecated  bool
	Tags        []string

	node       *yaml.Node // the operation mapping
	pathParams *yaml.Node // path-item level `parameters` sequence, or nil
}

// LoadOptions controls parsing.
type LoadOptions struct {
	// BasePath is the directory used to resolve relative external $refs.
	BasePath string
	// AllowRemoteRefs permits external $refs over http(s) during bundling.
	AllowRemoteRefs bool
}

// Load parses an OpenAPI 3.0 or 3.1 document. External $refs are bundled into
// local components, and 3.0 schema idioms are normalised to their 3.1 form, so
// everything downstream can assume a single self-contained 3.1 shape.
func Load(data []byte, opt LoadOptions) (*Spec, error) {
	data, err := bundleIfNeeded(data, opt)
	if err != nil {
		return nil, err
	}

	doc, err := libopenapi.NewDocument(data)
	if err != nil {
		return nil, fmt.Errorf("parse openapi document: %w", err)
	}
	// Building the typed model is how we validate that this is a structurally
	// sound v3 document. We then discard it and work on the raw tree.
	if _, err := doc.BuildV3Model(); err != nil {
		return nil, fmt.Errorf("invalid openapi v3 document: %w", err)
	}

	root := docRoot(doc.GetSpecInfo().RootNode)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("openapi document root is not a mapping")
	}

	s := &Spec{root: root}
	if v := mapGet(root, "openapi"); v != nil {
		s.Version = v.Value
	}
	if info := mapGet(root, "info"); info != nil {
		if v := mapGet(info, "title"); v != nil {
			s.Title = v.Value
		}
		if v := mapGet(info, "version"); v != nil {
			s.APIVersion = v.Value
		}
	}
	if s.Version == "" {
		return nil, fmt.Errorf("missing `openapi` version field")
	}
	if !strings.HasPrefix(s.Version, "3.") {
		return nil, fmt.Errorf("unsupported OpenAPI version %q: only 3.0.x and 3.1.x are supported", s.Version)
	}
	if strings.HasPrefix(s.Version, "3.0") {
		normalise30(root)
	}

	s.ops = s.collectOperations()
	return s, nil
}

// bundleIfNeeded flattens external $refs into local components. We skip the
// bundler entirely for self-contained documents, which is the common case and
// avoids paying for a full rolodex build on every spec refresh.
func bundleIfNeeded(data []byte, opt LoadOptions) ([]byte, error) {
	probe, err := libopenapi.NewDocument(data)
	if err != nil {
		return nil, fmt.Errorf("parse openapi document: %w", err)
	}
	var external bool
	walkRefs(docRoot(probe.GetSpecInfo().RootNode), func(ref *yaml.Node) {
		if !strings.HasPrefix(ref.Value, "#") {
			external = true
		}
	})
	if !external {
		return data, nil
	}

	cfg := &datamodel.DocumentConfiguration{
		BasePath:              opt.BasePath,
		AllowFileReferences:   true,
		AllowRemoteReferences: opt.AllowRemoteRefs,
	}
	out, err := bundler.BundleBytesComposed(data, cfg, nil)
	if err != nil {
		return nil, fmt.Errorf("bundle external $refs: %w", err)
	}
	return out, nil
}

// collectOperations walks paths -> path item -> method.
func (s *Spec) collectOperations() []*Operation {
	paths := mapGet(s.root, "paths")
	if paths == nil {
		return nil
	}
	var ops []*Operation
	for i := 0; i+1 < len(paths.Content); i += 2 {
		p, item := paths.Content[i].Value, paths.Content[i+1]
		if item.Kind != yaml.MappingNode {
			continue
		}
		pathParams := mapGet(item, "parameters")
		for j := 0; j+1 < len(item.Content); j += 2 {
			k, opNode := item.Content[j].Value, item.Content[j+1]
			if !isMethod(k) || opNode.Kind != yaml.MappingNode {
				continue
			}
			op := &Operation{
				Path:       p,
				Method:     strings.ToUpper(k),
				node:       opNode,
				pathParams: pathParams,
			}
			if v := mapGet(opNode, "operationId"); v != nil {
				op.OperationID = v.Value
			}
			if v := mapGet(opNode, "summary"); v != nil {
				op.Summary = v.Value
			}
			if v := mapGet(opNode, "deprecated"); v != nil {
				op.Deprecated = v.Value == "true"
			}
			if v := mapGet(opNode, "tags"); v != nil {
				for _, t := range v.Content {
					op.Tags = append(op.Tags, t.Value)
				}
			}
			if op.OperationID == "" {
				op.OperationID = syntheticOperationID(op.Method, p)
			}
			ops = append(ops, op)
		}
	}
	sort.SliceStable(ops, func(a, b int) bool {
		if ops[a].Path != ops[b].Path {
			return ops[a].Path < ops[b].Path
		}
		return ops[a].Method < ops[b].Method
	})
	return ops
}

// syntheticOperationID builds a stable id for operations that omit one. It has to
// be deterministic: it ends up in the published document and in metric labels.
func syntheticOperationID(method, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, seg := range strings.Split(path, "/") {
		if seg == "" {
			continue
		}
		seg = strings.Trim(seg, "{}")
		if seg == "" {
			continue
		}
		b.WriteByte('_')
		for _, r := range seg {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
		}
	}
	return b.String()
}

// Operations returns the spec's operations in a stable order.
func (s *Spec) Operations() []*Operation { return s.ops }

// FindOperation returns the operation with the given upstream operationId.
func (s *Spec) FindOperation(id string) *Operation {
	for _, op := range s.ops {
		if op.OperationID == id {
			return op
		}
	}
	return nil
}

// component returns the raw node for #/components/<section>/<name>.
func (s *Spec) component(section, name string) *yaml.Node {
	comps := mapGet(s.root, "components")
	if comps == nil {
		return nil
	}
	sec := mapGet(comps, section)
	if sec == nil {
		return nil
	}
	return mapGet(sec, name)
}

// SecuritySchemes returns the names declared under components.securitySchemes,
// in document order.
func (s *Spec) SecuritySchemes() []string {
	comps := mapGet(s.root, "components")
	if comps == nil {
		return nil
	}
	return mapKeys(mapGet(comps, "securitySchemes"))
}

// rootSecurity returns the document-level `security` node. An operation that
// declares none inherits this, so the merger has to consult it before deciding
// an upstream operation is unauthenticated.
func (s *Spec) rootSecurity() *yaml.Node { return mapGet(s.root, "security") }

// normalise30 rewrites OpenAPI 3.0 schema idioms into their 3.1 equivalents so
// the merged document is valid 3.1 and so request validation can assume JSON
// Schema 2020-12 semantics throughout.
//
// It deliberately handles only the constructs whose meaning would otherwise
// change: `nullable`, and the boolean form of `exclusiveMinimum`/`exclusiveMaximum`.
// Everything else is compatible between the two versions.
func normalise30(root *yaml.Node) {
	mapSet(root, "openapi", newScalar("3.1.0"))
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		if n.Kind == yaml.MappingNode {
			normaliseNullable(n)
			normaliseExclusive(n, "exclusiveMinimum", "minimum")
			normaliseExclusive(n, "exclusiveMaximum", "maximum")
			for i := 0; i+1 < len(n.Content); i += 2 {
				walk(n.Content[i+1])
			}
			return
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(root)
}

// normaliseNullable turns {type: string, nullable: true} into {type: [string, "null"]}.
func normaliseNullable(m *yaml.Node) {
	nullable := mapGet(m, "nullable")
	if nullable == nil || nullable.Value != "true" {
		return
	}
	mapDelete(m, "nullable")
	t := mapGet(m, "type")
	switch {
	case t == nil:
		// No type constraint: nullable adds nothing.
	case t.Kind == yaml.ScalarNode:
		seq := newSeq()
		seq.Content = append(seq.Content, newScalar(t.Value), newScalar("null"))
		mapSet(m, "type", seq)
	case t.Kind == yaml.SequenceNode:
		for _, c := range t.Content {
			if c.Value == "null" {
				return
			}
		}
		t.Content = append(t.Content, newScalar("null"))
	}
}

// normaliseExclusive turns {minimum: 5, exclusiveMinimum: true} into
// {exclusiveMinimum: 5}, which is the 3.1 / JSON Schema 2020-12 form.
func normaliseExclusive(m *yaml.Node, exclusiveKey, boundKey string) {
	ex := mapGet(m, exclusiveKey)
	if ex == nil || ex.Kind != yaml.ScalarNode {
		return
	}
	if ex.Value != "true" && ex.Value != "false" {
		return // already numeric: 3.1 form
	}
	bound := mapGet(m, boundKey)
	if ex.Value == "false" || bound == nil {
		mapDelete(m, exclusiveKey)
		return
	}
	mapSet(m, exclusiveKey, deepCopy(bound))
	mapDelete(m, boundKey)
}

func mapDelete(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
