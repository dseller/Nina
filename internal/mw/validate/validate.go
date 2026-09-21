// Package validate enforces the published OpenAPI contract on inbound requests.
//
// Only requests are validated. Response bodies are never parsed: the proxy
// streams them straight through, which keeps large downloads and chunked
// responses working and keeps upstream latency off the gateway's critical path.
package validate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"

	"github.com/rivencove/nina/internal/chain"
	"github.com/rivencove/nina/internal/httperr"
	"github.com/rivencove/nina/internal/oas"
	"github.com/rivencove/nina/internal/reqctx"
	"github.com/rivencove/nina/internal/routetable"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// printer renders schema error kinds. The library's own default printer is
// unexported and its LocalizedString panics on a nil one, so we supply ours.
var printer = message.NewPrinter(language.English)

// Mode decides what happens when a request does not match its schema.
type Mode string

const (
	ModeOff     Mode = "off"
	ModeWarn    Mode = "warn"    // record the violation, forward anyway
	ModeEnforce Mode = "enforce" // reject with 400
)

type settings struct {
	Request Mode  `json:"request"`
	MaxBody int64 `json:"max_body"`
	// UnknownQuery rejects query parameters the operation does not declare.
	// Off by default: clients add tracking parameters constantly and breaking
	// them is rarely what the operator meant.
	UnknownQuery bool `json:"unknown_query"`
}

// New constructs the validate middleware.
func New(name string, raw json.RawMessage, deps chain.Deps) (chain.Instance, error) {
	s := settings{Request: ModeEnforce, MaxBody: 1 << 20}
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		type alias settings
		var a struct {
			alias
			Type string `json:"type"`
		}
		a.alias = alias(s)
		if err := dec.Decode(&a); err != nil {
			return nil, err
		}
		s = settings(a.alias)
	}
	switch s.Request {
	case ModeOff, ModeWarn, ModeEnforce:
	default:
		return nil, fmt.Errorf("request must be off, warn or enforce, got %q", s.Request)
	}
	if s.MaxBody <= 0 {
		s.MaxBody = 1 << 20
	}
	return &instance{name: name, set: s, deps: deps, compilers: map[*oas.Spec]*compilerFor{}}, nil
}

type instance struct {
	name string
	set  settings
	deps chain.Deps

	mu        sync.Mutex
	compilers map[*oas.Spec]*compilerFor
}

// compilerFor holds one JSON Schema compiler per upstream document. The whole
// document is registered as a resource so that $refs between components resolve
// exactly as they do in the published spec.
type compilerFor struct {
	c   *jsonschema.Compiler
	err error
}

func (i *instance) Category() chain.Category { return chain.CatValidate }

func (i *instance) compiler(spec *oas.Spec) (*jsonschema.Compiler, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if cf, ok := i.compilers[spec]; ok {
		return cf.c, cf.err
	}
	cf := &compilerFor{}
	doc, err := spec.JSONDoc()
	if err != nil {
		cf.err = fmt.Errorf("render spec for validation: %w", err)
		i.compilers[spec] = cf
		return nil, cf.err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("nina:spec", doc); err != nil {
		cf.err = fmt.Errorf("register spec for validation: %w", err)
		i.compilers[spec] = cf
		return nil, cf.err
	}
	cf.c = c
	i.compilers[spec] = cf
	return c, nil
}

type compiledParam struct {
	oas.ParamSpec
	schema *jsonschema.Schema
}

type compiledRoute struct {
	set    settings
	params []compiledParam
	body   *compiledBody
	route  string
	log    *slog.Logger
}

type compiledBody struct {
	required bool
	// byType is keyed by lower-case media type without parameters.
	byType  map[string]*jsonschema.Schema
	maxBody int64
}

// ForRoute compiles this route's schemas once, at runtime-build time. Nothing
// here is compiled per request.
func (i *instance) ForRoute(r *routetable.Route) (chain.Middleware, error) {
	if i.set.Request == ModeOff {
		return nil, nil
	}
	params, body, err := r.Spec.ValidationSpec(r.Op)
	if err != nil {
		return nil, err
	}
	c, err := i.compiler(r.Spec)
	if err != nil {
		return nil, err
	}

	cr := &compiledRoute{set: i.set, route: r.OperationID, log: i.deps.Logger}
	for _, p := range params {
		cp := compiledParam{ParamSpec: p}
		if p.SchemaPtr != "" {
			sch, err := c.Compile("nina:spec#" + p.SchemaPtr)
			if err != nil {
				return nil, fmt.Errorf("compile schema for parameter %q: %w", p.Name, err)
			}
			cp.schema = sch
		}
		cr.params = append(cr.params, cp)
	}
	if body != nil && len(body.SchemaPtrs) > 0 {
		cb := &compiledBody{required: body.Required, byType: map[string]*jsonschema.Schema{}, maxBody: i.set.MaxBody}
		for mt, ptr := range body.SchemaPtrs {
			if !isJSONMediaType(mt) {
				// Only JSON bodies are validated; anything else passes through
				// untouched rather than being buffered and rejected.
				continue
			}
			sch, err := c.Compile("nina:spec#" + ptr)
			if err != nil {
				return nil, fmt.Errorf("compile schema for request body %q: %w", mt, err)
			}
			cb.byType[strings.ToLower(mt)] = sch
		}
		if len(cb.byType) > 0 {
			cr.body = cb
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cr.serve(w, r, next)
		})
	}, nil
}

func (cr *compiledRoute) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	var errs []httperr.FieldError

	info := reqctx.From(r.Context())
	errs = append(errs, cr.checkParams(r, info)...)

	if cr.body != nil {
		bodyErrs, fatal := cr.checkBody(w, r)
		if fatal {
			return
		}
		errs = append(errs, bodyErrs...)
	}

	if len(errs) == 0 {
		next.ServeHTTP(w, r)
		return
	}

	if cr.set.Request == ModeWarn {
		// Warn mode enforces nothing, so this is the only trace.
		if cr.log != nil {
			cr.log.WarnContext(r.Context(), "request does not match the API contract",
				"route", cr.route, "errors", len(errs), "enforced", false)
		}
		next.ServeHTTP(w, r)
		return
	}
	httperr.Write(w, r, httperr.Problem{
		Status: http.StatusBadRequest,
		Title:  "Request does not match the API contract",
		Detail: fmt.Sprintf("%d validation error(s)", len(errs)),
		Errors: errs,
	})
}

func (cr *compiledRoute) checkParams(r *http.Request, info *reqctx.Info) []httperr.FieldError {
	var errs []httperr.FieldError
	query := r.URL.Query()
	declared := map[string]bool{}

	for _, p := range cr.params {
		if p.In == "query" {
			declared[p.Name] = true
		}
		raw, present := extractParam(r, info, p, query)
		if !present {
			if p.Required {
				errs = append(errs, httperr.FieldError{
					In: p.In, Name: p.Name, Message: "required parameter is missing",
				})
			}
			continue
		}
		if p.schema == nil {
			continue
		}
		value := coerceParam(p, raw)
		if err := p.schema.Validate(value); err != nil {
			errs = append(errs, fieldErrors(p.In, p.Name, err)...)
		}
	}

	if cr.set.UnknownQuery {
		for name := range query {
			if !declared[name] {
				errs = append(errs, httperr.FieldError{
					In: "query", Name: name, Message: "unknown query parameter",
				})
			}
		}
	}
	return errs
}

// extractParam pulls a parameter's raw string values out of the request.
func extractParam(r *http.Request, info *reqctx.Info, p compiledParam, query map[string][]string) ([]string, bool) {
	switch p.In {
	case "path":
		if info == nil {
			return nil, false
		}
		v, ok := info.Params.Get(p.Name)
		if !ok {
			return nil, false
		}
		return []string{v}, true
	case "query":
		vs, ok := query[p.Name]
		if !ok {
			return nil, false
		}
		if p.Type == "array" && !p.Explode && len(vs) == 1 {
			// style=form, explode=false serialises an array as a,b,c.
			return strings.Split(vs[0], ","), true
		}
		return vs, true
	case "header":
		v := r.Header.Get(p.Name)
		if v == "" {
			if _, ok := r.Header[http.CanonicalHeaderKey(p.Name)]; !ok {
				return nil, false
			}
		}
		if p.Type == "array" {
			return strings.Split(v, ","), true
		}
		return []string{v}, true
	case "cookie":
		c, err := r.Cookie(p.Name)
		if err != nil {
			return nil, false
		}
		return []string{c.Value}, true
	}
	return nil, false
}

// coerceParam turns the string values HTTP carries into the JSON types the
// schema is written against.
func coerceParam(p compiledParam, raw []string) any {
	if p.Type == "array" {
		out := make([]any, 0, len(raw))
		for _, v := range raw {
			out = append(out, oas.Coerce(v, p.ItemType))
		}
		return out
	}
	if len(raw) == 0 {
		return nil
	}
	return oas.Coerce(raw[0], p.Type)
}

// checkBody validates a JSON request body and restores it for the proxy.
//
// The second return value reports that a response has already been written, so
// the caller must stop.
func (cr *compiledRoute) checkBody(w http.ResponseWriter, r *http.Request) ([]httperr.FieldError, bool) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		mt = ""
	}
	mt = strings.ToLower(mt)

	if r.Body == nil || r.ContentLength == 0 {
		if cr.body.required {
			return []httperr.FieldError{{In: "body", Message: "request body is required"}}, false
		}
		return nil, false
	}

	schema, ok := cr.body.byType[mt]
	if !ok {
		// Unvalidatable content type: forward it rather than inventing a rule the
		// published document does not state.
		return nil, false
	}

	limited := io.LimitReader(r.Body, cr.body.maxBody+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		httperr.Simple(w, r, http.StatusBadRequest, "could not read request body")
		return nil, true
	}
	if int64(len(data)) > cr.body.maxBody {
		httperr.Write(w, r, httperr.Problem{
			Status: http.StatusRequestEntityTooLarge,
			Title:  "Request body too large to validate",
			Detail: fmt.Sprintf("body exceeds the %d byte validation limit", cr.body.maxBody),
		})
		return nil, true
	}
	// Hand the buffered body back so the proxy can forward it.
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))

	if len(bytes.TrimSpace(data)) == 0 {
		if cr.body.required {
			return []httperr.FieldError{{In: "body", Message: "request body is required"}}, false
		}
		return nil, false
	}

	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return []httperr.FieldError{{In: "body", Message: "body is not valid JSON: " + err.Error()}}, false
	}
	if err := schema.Validate(v); err != nil {
		return fieldErrors("body", "", err), false
	}
	return nil, false
}

// fieldErrors flattens a validation error into the leaf causes, which are the
// ones that actually tell a caller what to fix.
func fieldErrors(in, name string, err error) []httperr.FieldError {
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []httperr.FieldError{{In: in, Name: name, Message: err.Error()}}
	}
	var out []httperr.FieldError
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			out = append(out, httperr.FieldError{
				In:      in,
				Name:    name,
				Pointer: "/" + strings.Join(e.InstanceLocation, "/"),
				Message: e.ErrorKind.LocalizedString(printer),
			})
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(out) == 0 {
		out = append(out, httperr.FieldError{In: in, Name: name, Message: ve.Error()})
	}
	// Keep the response bounded: a deeply wrong body can otherwise produce
	// hundreds of near-identical messages.
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func isJSONMediaType(mt string) bool {
	mt = strings.ToLower(strings.TrimSpace(mt))
	return mt == "application/json" || strings.HasSuffix(mt, "+json") ||
		strings.HasPrefix(mt, "application/json;")
}
