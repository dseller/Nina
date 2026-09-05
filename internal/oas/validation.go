package oas

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	yaml "go.yaml.in/yaml/v4"
	sigsyaml "sigs.k8s.io/yaml"
)

// ParamSpec describes one operation parameter well enough to decode a value out
// of the request and validate it.
type ParamSpec struct {
	Name     string
	In       string // path, query, header, cookie
	Required bool
	Style    string
	Explode  bool

	// SchemaPtr is a JSON pointer into the source document, so the validator can
	// compile the very schema that was published rather than a copy of it.
	SchemaPtr string
	// Type and ItemType come from the resolved schema and drive coercion of the
	// string values HTTP actually carries.
	Type     string
	ItemType string
}

// BodySpec describes a request body.
type BodySpec struct {
	Required bool
	// SchemaPtrs is keyed by media type, e.g. "application/json".
	SchemaPtrs map[string]string
}

// ValidationSpec extracts the parameter and body descriptors for an operation.
func (s *Spec) ValidationSpec(op *Operation) (params []ParamSpec, body *BodySpec, err error) {
	seen := map[string]bool{}

	add := func(node *yaml.Node, ptr string) error {
		p, err := s.paramSpec(node, ptr)
		if err != nil {
			return err
		}
		if p == nil {
			return nil
		}
		key := p.In + "\x00" + p.Name
		if seen[key] {
			return nil // operation level already won
		}
		seen[key] = true
		params = append(params, *p)
		return nil
	}

	opPtr := "/paths/" + escapePointer(op.Path) + "/" + strings.ToLower(op.Method)
	if ps := mapGet(op.node, "parameters"); ps != nil {
		for i, n := range ps.Content {
			if err := add(n, fmt.Sprintf("%s/parameters/%d", opPtr, i)); err != nil {
				return nil, nil, err
			}
		}
	}
	if op.pathParams != nil {
		base := "/paths/" + escapePointer(op.Path) + "/parameters"
		for i, n := range op.pathParams.Content {
			if err := add(n, fmt.Sprintf("%s/%d", base, i)); err != nil {
				return nil, nil, err
			}
		}
	}

	if rb := mapGet(op.node, "requestBody"); rb != nil {
		ptr := opPtr + "/requestBody"
		node := rb
		if ref := mapGet(rb, "$ref"); ref != nil {
			resolved, rptr, ok := s.follow(ref.Value)
			if !ok {
				return nil, nil, fmt.Errorf("operation %s: unresolved requestBody $ref %q", op.OperationID, ref.Value)
			}
			node, ptr = resolved, rptr
		}
		b := &BodySpec{SchemaPtrs: map[string]string{}}
		if req := mapGet(node, "required"); req != nil {
			b.Required = req.Value == "true"
		}
		if content := mapGet(node, "content"); content != nil {
			for i := 0; i+1 < len(content.Content); i += 2 {
				mt, mtNode := content.Content[i].Value, content.Content[i+1]
				if mapGet(mtNode, "schema") == nil {
					continue
				}
				b.SchemaPtrs[mt] = ptr + "/content/" + escapePointer(mt) + "/schema"
			}
		}
		body = b
	}
	return params, body, nil
}

// paramSpec resolves one parameter node, following a $ref if present.
func (s *Spec) paramSpec(node *yaml.Node, ptr string) (*ParamSpec, error) {
	if ref := mapGet(node, "$ref"); ref != nil {
		resolved, rptr, ok := s.follow(ref.Value)
		if !ok {
			return nil, fmt.Errorf("unresolved parameter $ref %q", ref.Value)
		}
		node, ptr = resolved, rptr
	}
	name := mapGet(node, "name")
	in := mapGet(node, "in")
	if name == nil || in == nil {
		return nil, nil // not a usable parameter; the document builder already validated shape
	}
	p := &ParamSpec{Name: name.Value, In: in.Value}
	if req := mapGet(node, "required"); req != nil {
		p.Required = req.Value == "true"
	}
	if p.In == "path" {
		p.Required = true // path parameters are always required
	}
	if st := mapGet(node, "style"); st != nil {
		p.Style = st.Value
	}
	if p.Style == "" {
		p.Style = defaultStyle(p.In)
	}
	// explode defaults to true for form style and false otherwise.
	p.Explode = p.Style == "form"
	if ex := mapGet(node, "explode"); ex != nil {
		p.Explode = ex.Value == "true"
	}
	if sch := mapGet(node, "schema"); sch != nil {
		p.SchemaPtr = ptr + "/schema"
		resolved := s.resolveSchema(sch)
		p.Type = schemaType(resolved)
		if p.Type == "array" {
			p.ItemType = schemaType(s.resolveSchema(mapGet(resolved, "items")))
		}
	}
	return p, nil
}

func defaultStyle(in string) string {
	switch in {
	case "query", "cookie":
		return "form"
	default: // path, header
		return "simple"
	}
}

// resolveSchema follows $ref chains to the concrete schema node.
func (s *Spec) resolveSchema(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && i < 32; i++ {
		ref := mapGet(n, "$ref")
		if ref == nil {
			return n
		}
		next, _, ok := s.follow(ref.Value)
		if !ok {
			return n
		}
		n = next
	}
	return n
}

// follow resolves a local components pointer to its node and canonical pointer.
func (s *Spec) follow(ref string) (*yaml.Node, string, bool) {
	section, name, tail, ok := parseLocalRef(ref)
	if !ok || tail != "" {
		return nil, "", false
	}
	n := s.component(section, name)
	if n == nil {
		return nil, "", false
	}
	return n, "/components/" + escapePointer(section) + "/" + escapePointer(name), true
}

// schemaType returns the primary type, treating a 3.1 union like ["string","null"]
// as its non-null member so coercion still knows what to attempt.
func schemaType(n *yaml.Node) string {
	t := mapGet(n, "type")
	if t == nil {
		return ""
	}
	if t.Kind == yaml.ScalarNode {
		return t.Value
	}
	for _, c := range t.Content {
		if c.Value != "null" {
			return c.Value
		}
	}
	return ""
}

// JSONDoc renders the whole document as a decoded JSON value, which is what a
// JSON Schema compiler wants as a resource so that $refs resolve.
func (s *Spec) JSONDoc() (any, error) {
	y, err := yaml.Marshal(s.root)
	if err != nil {
		return nil, err
	}
	j, err := sigsyaml.YAMLToJSON(y)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(j, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// Coerce converts a string carried by HTTP into the JSON type the schema
// expects, so that "42" validates against {type: integer}.
//
// A value that cannot be converted is returned as the original string: letting
// the schema reject it produces a far better message than a coercion error.
func Coerce(v, typ string) any {
	switch typ {
	case "integer":
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	case "number":
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	case "boolean":
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return v
}
