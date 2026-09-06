package oas

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	yaml "go.yaml.in/yaml/v4"
	sigsyaml "sigs.k8s.io/yaml"
)

// SpecSource is one upstream document participating in a merge, together with the
// namespace its components are published under.
type SpecSource struct {
	Spec *Spec
	// Backend is the configured backend name, used in error messages and in the
	// x-nina-backend extension.
	Backend string
	// Namespace prefixes component names, e.g. "Users" turns User into UsersUser.
	Namespace string
	// SchemeNames maps a security scheme's upstream name to the name it should
	// be published under. A chosen name is used verbatim, without Namespace.
	SchemeNames map[string]string
}

// pinnedName returns the operator-chosen published name for a component, if
// there is one. Only security schemes can be named this way: their names are
// what a documentation viewer labels its credential box with, so they are the
// one part of the component namespace worth exposing to configuration.
func (s *SpecSource) pinnedName(section, name string) (string, bool) {
	if section != "securitySchemes" || len(s.SchemeNames) == 0 {
		return "", false
	}
	published, ok := s.SchemeNames[name]
	if !ok || published == "" {
		return "", false
	}
	return published, true
}

// ExposedOp is a single upstream operation as it will be published by the gateway.
type ExposedOp struct {
	Source      *SpecSource
	Op          *Operation
	GatewayPath string // e.g. /users/{id}
	OperationID string // gateway-level id, e.g. users_getUser
}

// MergeOptions describes the gateway-level document being produced.
type MergeOptions struct {
	Title       string
	Version     string
	Description string
	Servers     []string
	// SecuritySchemes are published under components.securitySchemes and are
	// derived from the auth middleware the gateway actually enforces.
	SecuritySchemes map[string]*yaml.Node
	// Security is the document-level security requirement list.
	Security []map[string][]string
	// Dedupe collapses structurally identical components that share a base name.
	Dedupe bool
}

// CollisionError reports two backends claiming the same gateway route.
type CollisionError struct {
	Method, Path string
	A, B         string // "backend.operationId" for each claimant
}

func (e *CollisionError) Error() string {
	return fmt.Sprintf("route collision: %s %s is claimed by both %s and %s", e.Method, e.Path, e.A, e.B)
}

// Merge renders the exposed operations into a single OpenAPI 3.1 document.
//
// Component names are namespaced per source so that collisions are impossible by
// construction; route collisions, which namespacing cannot fix, are a hard error.
func Merge(ops []ExposedOp, opt MergeOptions) (*yaml.Node, error) {
	m := &merger{
		opt:         opt,
		imported:    map[compKey]string{},
		comps:       map[string]map[string]*importedComp{},
		specSources: map[*Spec]*SpecSource{},
	}
	return m.run(ops)
}

type compKey struct {
	spec    *Spec
	section string
	name    string
}

type importedComp struct {
	node     *yaml.Node
	baseName string // original name in the source document
	key      compKey
	// pinned marks a name chosen by configuration rather than derived. Such a
	// name is never rewritten: the dedupe pass leaves it alone, and a clash with
	// it is an error rather than something to suffix around.
	pinned bool
}

// claim records which operation node occupies a (path, method) slot, and who put
// it there, so a collision can name both claimants.
type claim struct {
	node *yaml.Node
	by   string
}

type merger struct {
	opt      MergeOptions
	imported map[compKey]string                  // source component -> published name
	comps    map[string]map[string]*importedComp // section -> published name -> component
	queue    []compKey
	// specSources remembers the namespace each spec was introduced under, so
	// components discovered transitively resolve under the right prefix.
	specSources map[*Spec]*SpecSource
}

func (m *merger) run(ops []ExposedOp) (*yaml.Node, error) {
	pathOps := map[string]map[string]claim{}

	sorted := append([]ExposedOp(nil), ops...)
	sort.SliceStable(sorted, func(a, b int) bool {
		if sorted[a].GatewayPath != sorted[b].GatewayPath {
			return sorted[a].GatewayPath < sorted[b].GatewayPath
		}
		return sorted[a].Op.Method < sorted[b].Op.Method
	})

	for _, e := range sorted {
		method := strings.ToLower(e.Op.Method)
		who := e.Source.Backend + "." + e.Op.OperationID
		if byMethod, ok := pathOps[e.GatewayPath]; ok {
			if prev, ok := byMethod[method]; ok {
				return nil, &CollisionError{
					Method: e.Op.Method, Path: e.GatewayPath, A: prev.by, B: who,
				}
			}
		} else {
			pathOps[e.GatewayPath] = map[string]claim{}
		}

		node, err := m.buildOperation(e)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", who, err)
		}
		pathOps[e.GatewayPath][method] = claim{node: node, by: who}
	}

	// Import the transitive component closure discovered while copying operations.
	if err := m.drainQueue(); err != nil {
		return nil, err
	}

	aliases := map[string]string{}
	if m.opt.Dedupe {
		aliases = m.dedupe()
	}
	m.applyAliases(aliases)
	schemeRenames := schemeAliases(aliases)
	for _, byMethod := range pathOps {
		for _, c := range byMethod {
			rewriteRefs(c.node, aliases)
			renameSecuritySchemes(c.node, schemeRenames)
		}
	}

	return m.assemble(pathOps), nil
}

// buildOperation copies one upstream operation and rewrites it for publication:
// path-item parameters are folded in, the operationId is replaced with the
// gateway-level id, provenance extensions are attached, and every $ref is
// repointed at the namespaced component.
func (m *merger) buildOperation(e ExposedOp) (*yaml.Node, error) {
	node := deepCopy(e.Op.node)

	if e.Op.pathParams != nil {
		if err := foldPathParams(node, e.Op.pathParams); err != nil {
			return nil, err
		}
	}

	mapSet(node, "operationId", newScalar(e.OperationID))
	mapSet(node, "x-nina-backend", newScalar(e.Source.Backend))
	mapSet(node, "x-nina-upstream-path", newScalar(e.Op.Path))
	mapSet(node, "x-nina-upstream-operation-id", newScalar(e.Op.OperationID))

	if err := m.rewriteSecurity(e.Source, node); err != nil {
		return nil, err
	}

	rename := map[string]string{}
	var err error
	walkRefs(node, func(ref *yaml.Node) {
		if err != nil {
			return
		}
		var nr string
		nr, err = m.resolveRef(e.Source, ref.Value)
		if err == nil {
			rename[ref.Value] = nr
		}
	})
	if err != nil {
		return nil, err
	}
	rewriteRefs(node, rename)
	return node, nil
}

// resolveRef registers the referenced component for import and returns the
// rewritten pointer.
func (m *merger) resolveRef(src *SpecSource, ref string) (string, error) {
	section, name, tail, ok := parseLocalRef(ref)
	if !ok {
		return "", fmt.Errorf("unsupported $ref %q: only local #/components/... pointers are supported after bundling", ref)
	}
	published, err := m.enqueue(src, section, name)
	if err != nil {
		return "", err
	}
	out := componentRef(section, published)
	if tail != "" {
		out += "/" + tail
	}
	return out, nil
}

// rewriteSecurity republishes an operation's security requirements against the
// gateway's namespaced securitySchemes.
//
// A requirement names a scheme rather than $ref-ing it, so the $ref walk never
// sees one. Without this the merged document carries requirement names pointing
// at schemes that were never imported: a documentation viewer then shows an
// operation as authenticated but offers no way to supply the credential, which
// is precisely the failure this exists to prevent.
//
// An operation that states no requirement of its own inherits the source
// document's, because the merged document's own top-level `security` describes
// the gateway, not this upstream.
func (m *merger) rewriteSecurity(src *SpecSource, op *yaml.Node) error {
	sec := mapGet(op, "security")
	if sec == nil {
		if src.Spec == nil {
			return nil
		}
		sec = deepCopy(src.Spec.rootSecurity())
		if sec == nil {
			return nil
		}
	}
	if sec.Kind != yaml.SequenceNode {
		return fmt.Errorf("`security` is not a sequence")
	}

	out := newSeq()
	for _, req := range sec.Content {
		if req.Kind != yaml.MappingNode {
			return fmt.Errorf("`security` entry is not a mapping")
		}
		entry := newMap()
		unresolved := false
		for i := 0; i+1 < len(req.Content); i += 2 {
			name, scopes := req.Content[i].Value, req.Content[i+1]
			published, ok, err := m.enqueueScheme(src, name)
			if err != nil {
				return err
			}
			if !ok {
				unresolved = true
				continue
			}
			mapSet(entry, published, deepCopy(scopes))
		}
		if unresolved && len(entry.Content) == 0 {
			// Every scheme in this alternative names something the upstream never
			// declared. Publishing the empty mapping would say "no authentication
			// required", a far stronger claim than the upstream made, so the
			// alternative is dropped instead.
			continue
		}
		out.Content = append(out.Content, entry)
	}

	if len(out.Content) == 0 {
		mapDelete(op, "security")
		return nil
	}
	mapSet(op, "security", out)
	return nil
}

// enqueueScheme is enqueue for a security scheme, reporting absence rather than
// failing the merge. A requirement naming an undeclared scheme is a flaw in the
// upstream document that the gateway cannot repair, and refusing to serve every
// other route because of it would be a poor trade. Every other failure — a name
// two backends both claim, say — is a real one and still stops the merge.
func (m *merger) enqueueScheme(src *SpecSource, name string) (string, bool, error) {
	if src.Spec == nil || src.Spec.component("securitySchemes", name) == nil {
		return "", false, nil
	}
	published, err := m.enqueue(src, "securitySchemes", name)
	if err != nil {
		return "", false, err
	}
	return published, true, nil
}

// enqueue reserves a published name for a source component and schedules it for
// copying. Names are namespaced, so two sources can never contend for one name.
func (m *merger) enqueue(src *SpecSource, section, name string) (string, error) {
	if _, ok := m.specSources[src.Spec]; !ok {
		m.specSources[src.Spec] = src
	}
	key := compKey{spec: src.Spec, section: section, name: name}
	if published, ok := m.imported[key]; ok {
		return published, nil
	}
	node := src.Spec.component(section, name)
	if node == nil {
		return "", fmt.Errorf("dangling $ref: %s has no components.%s.%s", src.Backend, section, name)
	}
	if m.comps[section] == nil {
		m.comps[section] = map[string]*importedComp{}
	}

	if pinned, ok := src.pinnedName(section, name); ok {
		if existing, taken := m.comps[section][pinned]; taken {
			if !m.sameComponent(existing, src.Spec, section, name) {
				return "", fmt.Errorf(
					"security scheme name %q is claimed by both %s.%s and %s.%s, and the two are not identical: "+
						"give them different names, or leave one to be namespaced",
					pinned, m.sourceFor(existing.key.spec).Backend, existing.key.name, src.Backend, name)
			}
			// Two backends declaring the same scheme and choosing the same name
			// for it: publish it once and point both at it. This is how an
			// operator says "these really are the same credential".
			m.imported[key] = pinned
			return pinned, nil
		}
		m.imported[key] = pinned
		m.comps[section][pinned] = &importedComp{baseName: name, key: key, pinned: true}
		m.queue = append(m.queue, key)
		return pinned, nil
	}

	published := src.Namespace + capitalise(name)
	// Namespacing makes collisions vanishingly unlikely, but two different source
	// names can still normalise to the same published name (e.g. "user" and
	// "User"). Disambiguate rather than silently overwrite.
	for n := 2; ; n++ {
		if _, taken := m.comps[section][published]; !taken {
			break
		}
		published = src.Namespace + capitalise(name) + strconv.Itoa(n)
	}
	m.imported[key] = published
	m.comps[section][published] = &importedComp{baseName: name, key: key}
	m.queue = append(m.queue, key)
	return published, nil
}

// sameComponent reports whether an already-imported component and a candidate
// from another document are structurally identical. It compares the source nodes
// rather than the imported ones because it runs during enqueue, before anything
// has been copied.
func (m *merger) sameComponent(existing *importedComp, spec *Spec, section, name string) bool {
	a := existing.key.spec.component(existing.key.section, existing.key.name)
	b := spec.component(section, name)
	return canonicalRaw(a) == canonicalRaw(b)
}

// canonicalRaw renders a node to a stable string, sorting mapping keys so that
// key order does not affect equality. Unlike merger.canonical it does not follow
// $refs: it compares documents whose components have not been imported yet, and
// a security scheme has no $refs to follow in any case.
func canonicalRaw(n *yaml.Node) string {
	if n == nil {
		return "~"
	}
	switch n.Kind {
	case yaml.MappingNode:
		type kv struct{ k, v string }
		items := make([]kv, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			items = append(items, kv{n.Content[i].Value, canonicalRaw(n.Content[i+1])})
		}
		sort.Slice(items, func(a, b int) bool { return items[a].k < items[b].k })
		var b strings.Builder
		b.WriteByte('{')
		for _, it := range items {
			b.WriteString(it.k)
			b.WriteByte(':')
			b.WriteString(it.v)
			b.WriteByte(',')
		}
		b.WriteByte('}')
		return b.String()
	case yaml.SequenceNode:
		var b strings.Builder
		b.WriteByte('[')
		for _, c := range n.Content {
			b.WriteString(canonicalRaw(c))
			b.WriteByte(',')
		}
		b.WriteByte(']')
		return b.String()
	default:
		return n.Tag + "|" + n.Value
	}
}

// drainQueue copies queued components, discovering further components as it goes.
func (m *merger) drainQueue() error {
	for i := 0; i < len(m.queue); i++ {
		key := m.queue[i]
		published := m.imported[key]
		ic := m.comps[key.section][published]
		if ic.node != nil {
			continue
		}
		src := m.sourceFor(key.spec)
		node := deepCopy(key.spec.component(key.section, key.name))
		rename := map[string]string{}
		var err error
		walkRefs(node, func(ref *yaml.Node) {
			if err != nil {
				return
			}
			var nr string
			nr, err = m.resolveRef(src, ref.Value)
			if err == nil {
				rename[ref.Value] = nr
			}
		})
		if err != nil {
			return fmt.Errorf("components.%s.%s: %w", key.section, key.name, err)
		}
		rewriteRefs(node, rename)
		ic.node = node
		ic.key = key
	}
	m.queue = nil
	return nil
}

// sourceFor returns the namespace context a spec was introduced under.
func (m *merger) sourceFor(s *Spec) *SpecSource {
	if src, ok := m.specSources[s]; ok {
		return src
	}
	return &SpecSource{Spec: s}
}

// rewriteRefs replaces $ref values using the supplied mapping.
func rewriteRefs(n *yaml.Node, rename map[string]string) {
	if len(rename) == 0 {
		return
	}
	walkRefs(n, func(ref *yaml.Node) {
		if nv, ok := rename[ref.Value]; ok {
			ref.Value = nv
			ref.Style = 0
		}
	})
}

// schemeAliases reduces the dedupe pass's ref renames to the securityScheme name
// renames among them, keyed by published name.
func schemeAliases(aliases map[string]string) map[string]string {
	out := map[string]string{}
	for from, to := range aliases {
		fromSec, fromName, fromTail, ok := parseLocalRef(from)
		if !ok || fromSec != "securitySchemes" || fromTail != "" {
			continue
		}
		_, toName, _, ok := parseLocalRef(to)
		if !ok {
			continue
		}
		out[fromName] = toName
	}
	return out
}

// renameSecuritySchemes updates security requirement names after a dedupe pass.
// Requirements hold plain names rather than $refs, so rewriteRefs cannot reach
// them and a collapsed scheme would otherwise leave the requirement dangling.
func renameSecuritySchemes(op *yaml.Node, renames map[string]string) {
	if len(renames) == 0 {
		return
	}
	sec := mapGet(op, "security")
	if sec == nil || sec.Kind != yaml.SequenceNode {
		return
	}
	for _, req := range sec.Content {
		if req.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(req.Content); i += 2 {
			if nv, ok := renames[req.Content[i].Value]; ok {
				req.Content[i].Value = nv
				req.Content[i].Style = 0
			}
		}
	}
}

// foldPathParams merges path-item level parameters into the operation. Operation
// level parameters win on a (name, in) match, per the specification. We fold
// rather than emit a path-level list because one gateway path can gather
// operations from different upstream path items.
func foldPathParams(op *yaml.Node, pathParams *yaml.Node) error {
	if pathParams.Kind != yaml.SequenceNode {
		return fmt.Errorf("path-level `parameters` is not a sequence")
	}
	existing := map[string]bool{}
	opParams := mapGet(op, "parameters")
	if opParams != nil {
		for _, p := range opParams.Content {
			existing[paramIdentity(p)] = true
		}
	}
	var add []*yaml.Node
	for _, p := range pathParams.Content {
		if id := paramIdentity(p); id != "" && existing[id] {
			continue
		}
		add = append(add, deepCopy(p))
	}
	if len(add) == 0 {
		return nil
	}
	if opParams == nil {
		opParams = newSeq()
		mapSet(op, "parameters", opParams)
	}
	// Path-level parameters come first, matching how authors read them.
	opParams.Content = append(add, opParams.Content...)
	return nil
}

// paramIdentity returns "name\x00in" for a concrete parameter. A $ref parameter
// has no identity we can compare without resolving it, so it never suppresses a
// path-level entry; duplicates there are harmless.
func paramIdentity(p *yaml.Node) string {
	name := mapGet(p, "name")
	in := mapGet(p, "in")
	if name == nil || in == nil {
		return ""
	}
	return name.Value + "\x00" + in.Value
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 32
	}
	return string(r)
}

// ---------------------------------------------------------------------------
// Dedupe
// ---------------------------------------------------------------------------

// dedupe finds components that are structurally identical and share a base name,
// and collapses them onto that shared base name. Without this, merging eight
// backends yields eight near-identical Error schemas and an unreadable document.
//
// Equality is structural through $refs: two components match only when their
// entire reachable subgraphs match, so UsersUser and OrdersUser do not collapse
// merely because both have an `id` field pointing at differently-shaped types.
func (m *merger) dedupe() map[string]string {
	type groupKey struct{ section, base, hash string }
	groups := map[groupKey][]string{}

	for section, byName := range m.comps {
		for name, ic := range byName {
			if ic.pinned {
				continue // an operator chose this name; it is not ours to collapse
			}
			h := m.hashComponent(section, name, nil)
			k := groupKey{section: section, base: ic.baseName, hash: h}
			groups[k] = append(groups[k], name)
		}
	}

	// A base name is only safe to claim when every component that wants it agrees
	// on structure. Count distinct hashes per (section, base).
	hashesPerBase := map[[2]string]map[string]bool{}
	for k := range groups {
		b := [2]string{k.section, k.base}
		if hashesPerBase[b] == nil {
			hashesPerBase[b] = map[string]bool{}
		}
		hashesPerBase[b][k.hash] = true
	}

	aliases := map[string]string{}
	for k, names := range groups {
		if len(names) < 2 {
			continue
		}
		if len(hashesPerBase[[2]string{k.section, k.base}]) != 1 {
			continue // the base name is contested; keep everyone namespaced
		}
		target := capitalise(k.base)
		if _, taken := m.comps[k.section][target]; taken {
			continue // an un-namespaced component already owns this name
		}
		sort.Strings(names)
		keep := m.comps[k.section][names[0]]
		for _, n := range names {
			aliases[componentRef(k.section, n)] = componentRef(k.section, target)
			delete(m.comps[k.section], n)
		}
		m.comps[k.section][target] = keep
	}
	return aliases
}

// hashComponent computes a structural fingerprint, following $refs into their
// targets. stack carries the components currently being hashed so that recursive
// schemas terminate; a cycle is encoded by its depth offset, which keeps
// isomorphic cycles equal.
func (m *merger) hashComponent(section, name string, stack []string) string {
	self := section + "/" + name
	for i, s := range stack {
		if s == self {
			return "cycle:" + strconv.Itoa(len(stack)-i)
		}
	}
	ic := m.comps[section][name]
	if ic == nil || ic.node == nil {
		return "missing:" + self
	}
	stack = append(stack, self)
	sum := sha256.Sum256([]byte(m.canonical(ic.node, stack)))
	return hex.EncodeToString(sum[:8])
}

// canonical renders a node to a stable string. Mapping keys are sorted so that
// key order does not affect equality, and $ref values are replaced by the hash
// of what they point at.
func (m *merger) canonical(n *yaml.Node, stack []string) string {
	if n == nil {
		return "~"
	}
	switch n.Kind {
	case yaml.MappingNode:
		type kv struct{ k, v string }
		var items []kv
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1]
			if k == "$ref" && v.Kind == yaml.ScalarNode {
				sec, nm, tail, ok := parseLocalRef(v.Value)
				if ok {
					items = append(items, kv{k, "ref(" + m.hashComponent(sec, nm, stack) + "/" + tail + ")"})
					continue
				}
			}
			items = append(items, kv{k, m.canonical(v, stack)})
		}
		sort.Slice(items, func(a, b int) bool { return items[a].k < items[b].k })
		var b strings.Builder
		b.WriteByte('{')
		for _, it := range items {
			b.WriteString(it.k)
			b.WriteByte(':')
			b.WriteString(it.v)
			b.WriteByte(',')
		}
		b.WriteByte('}')
		return b.String()
	case yaml.SequenceNode:
		var b strings.Builder
		b.WriteByte('[')
		for _, c := range n.Content {
			b.WriteString(m.canonical(c, stack))
			b.WriteByte(',')
		}
		b.WriteByte(']')
		return b.String()
	default:
		return n.Tag + "|" + n.Value
	}
}

// applyAliases rewrites refs inside retained components after a dedupe pass.
func (m *merger) applyAliases(aliases map[string]string) {
	if len(aliases) == 0 {
		return
	}
	for _, byName := range m.comps {
		for _, ic := range byName {
			rewriteRefs(ic.node, aliases)
		}
	}
}

// ---------------------------------------------------------------------------
// Assembly
// ---------------------------------------------------------------------------

func (m *merger) assemble(pathOps map[string]map[string]claim) *yaml.Node {
	root := newMap()
	mapSet(root, "openapi", newScalar("3.1.0"))

	info := newMap()
	mapSet(info, "title", newScalar(orDefault(m.opt.Title, "API Gateway")))
	mapSet(info, "version", newScalar(orDefault(m.opt.Version, "0.0.0")))
	if m.opt.Description != "" {
		mapSet(info, "description", newScalar(m.opt.Description))
	}
	mapSet(root, "info", info)

	if len(m.opt.Servers) > 0 {
		servers := newSeq()
		for _, u := range m.opt.Servers {
			s := newMap()
			mapSet(s, "url", newScalar(u))
			servers.Content = append(servers.Content, s)
		}
		mapSet(root, "servers", servers)
	}

	if len(m.opt.Security) > 0 {
		sec := newSeq()
		for _, req := range m.opt.Security {
			entry := newMap()
			names := make([]string, 0, len(req))
			for n := range req {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				scopes := newSeq()
				for _, s := range req[n] {
					scopes.Content = append(scopes.Content, newScalar(s))
				}
				mapSet(entry, n, scopes)
			}
			sec.Content = append(sec.Content, entry)
		}
		mapSet(root, "security", sec)
	}

	paths := newMap()
	pathNames := make([]string, 0, len(pathOps))
	for p := range pathOps {
		pathNames = append(pathNames, p)
	}
	sort.Strings(pathNames)
	for _, p := range pathNames {
		item := newMap()
		for _, method := range httpMethods {
			if c, ok := pathOps[p][method]; ok {
				mapSet(item, method, c.node)
			}
		}
		mapSet(paths, p, item)
	}
	mapSet(root, "paths", paths)

	comps := newMap()
	// Sections in the order the specification lists them, so the output is stable
	// and reads like a hand-written document.
	for _, section := range []string{
		"schemas", "responses", "parameters", "examples", "requestBodies",
		"headers", "securitySchemes", "links", "callbacks", "pathItems",
	} {
		byName := m.comps[section]
		if len(byName) == 0 && section != "securitySchemes" {
			continue
		}
		sec := newMap()
		names := make([]string, 0, len(byName))
		for n := range byName {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			mapSet(sec, n, byName[n].node)
		}
		if section == "securitySchemes" {
			schemeNames := make([]string, 0, len(m.opt.SecuritySchemes))
			for n := range m.opt.SecuritySchemes {
				schemeNames = append(schemeNames, n)
			}
			sort.Strings(schemeNames)
			for _, n := range schemeNames {
				mapSet(sec, n, m.opt.SecuritySchemes[n])
			}
		}
		if len(sec.Content) > 0 {
			mapSet(comps, section, sec)
		}
	}
	if len(comps.Content) > 0 {
		mapSet(root, "components", comps)
	}
	return root
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Render serialises a merged document to YAML.
func Render(root *yaml.Node) ([]byte, error) {
	return yaml.Marshal(root)
}

// RenderJSON serialises a merged document to indented JSON, for consumers and
// tooling that will not accept YAML.
func RenderJSON(root *yaml.Node) ([]byte, error) {
	y, err := yaml.Marshal(root)
	if err != nil {
		return nil, err
	}
	j, err := sigsyaml.YAMLToJSON(y)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, j, "", "  "); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
