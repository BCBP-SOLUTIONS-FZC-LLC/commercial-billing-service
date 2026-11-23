package http

import (
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// OpenAPI 3.1 document of the HTTP API (§5), generated from the route
// table and the request/response types, so it cannot drift from the code.
// Served at GET /openapi.json; docs/swagger/openapi.json is the committed
// copy, kept equal by TestOpenAPIGolden (`go test ./internal/adapters/http -run Golden -update`).

// summaries are the §5.3 "Purpose" column.
var summaries = map[string]string{
	"B-P1": "Subscription summary for the Billing page", "B-P2": "Hosted checkout to convert a trial (§8.1)",
	"B-P3": "Price of an upgrade now / effect date of a downgrade", "B-P4": "Upgrade now (prorated) or schedule a downgrade (period end)",
	"B-P5": "Cancel a scheduled downgrade", "B-P6": "Provider proration preview for added seats", "B-P7": "Buy seats (§8.5)",
	"B-P8": "Seat purchase status", "B-P9": "Seat purchase history", "B-P10": "Schedule (or replace) a reduction for the next renewal",
	"B-P11": "Cancel the scheduled reduction", "B-P12": "Cancel at period end (auto-renew off)", "B-P13": "Undo a pending cancellation",
	"B-P14": "Settle a past_due or cancelled subscription (§8.9)", "B-P15": "Payment methods, read live from the provider",
	"B-P16": "Provider-hosted page to add or replace the default payment method", "B-P17": "Invoice list",
	"B-P18": "302 to a short-lived provider PDF link", "B-P19": "Active token packs and this tenant's eligibility",
	"B-P20": "Buy token packs (§8.7)", "B-P21": "Add-on purchase status", "B-P22": "Add-on purchase history",
	"B-P23": "Hosted payment page when the off-session charge needs the customer", "B-P24": "Status, plan and seat history",
	"B-P25": "Billing contact, company, country, tax id", "B-P26": "Update the billing contact",
	"B-I1": "Direct paid signup: create account and subscription, charge (Signup BFF only)",
	"B-O1": "Phase 1 conversion stub", "B-O2": "Credit or debit on the customer balance", "B-O3": "Refund all or part of a paid invoice",
	"B-O4": "Invoice list including void and uncollectible", "B-O5": "Start a provider reconciliation run", "B-O6": "Reconciliation run status and findings",
	"B-O7": "Full subscription, sagas, purchases, schedules, disputes, ledger excerpt", "B-O8": "Operator-approved RP-4 reversal (§8.2)",
	"B-O9": "Stuck saga: retry, refund, resolved_manually", "B-O10": "Pause or continue billing (mandatory reason)",
	"B-O11": "Add a price row (closes the current one)", "B-O12": "Add, change or retire a token pack", "B-O13": "Re-process a ledger row (§20.2)",
	"B-O14": "Record a certification run produced by cmd/certify", "B-O15": "Revoke a certification",
	"B-O16": "Finalise or void a draft held during a collection pause", "B-O17": "Approve or reject a submitted certification run (pci_owner_reviewer)",
}

// queryTypes are the routes with query parameters.
var queryTypes = map[string]reflect.Type{
	"B-P9": typeOf[app.Page](), "B-P22": typeOf[app.Page](), "B-P24": typeOf[app.Page](),
	"B-P17": typeOf[app.InvoiceFilter](), "B-O4": typeOf[app.InvoiceFilter](),
}

type obj = map[string]any

// OpenAPI returns the document.
func OpenAPI() []byte {
	g := &schemaGen{defs: obj{}}
	paths := obj{}
	defs := append(append(append([]routeDef{}, tenantRoutes...), internalRoute), operatorRoutes...)
	for _, d := range defs {
		path, params := openAPIPath(d.Path)
		item, _ := paths[path].(obj)
		if item == nil {
			item = obj{}
			paths[path] = item
		}
		item[strings.ToLower(d.Method)] = g.operation(d, params)
	}
	doc := obj{
		"openapi": "3.1.0",
		"info": obj{"title": "commercial-billing API", "version": "1.0.0",
			"description": "Billing's HTTP API (LLD §5). Identity comes only from gateway- or mesh-injected x-user-id, x-tenant-id and " +
				"x-tenant-roles (§5.1); errors are RFC 9457 problems (application/problem+json) with the §17 errorCode. " +
				"B-W1 (provider webhooks) is documented in §5.4.3."},
		"paths":      paths,
		"components": obj{"schemas": g.withProblem()},
	}
	out, _ := json.MarshalIndent(doc, "", "  ")
	return append(out, '\n')
}

var pathParam = regexp.MustCompile(`:([a-z_]+)`)

func openAPIPath(p string) (string, []string) {
	var names []string
	out := pathParam.ReplaceAllStringFunc(p, func(m string) string {
		names = append(names, m[1:])
		return "{" + m[1:] + "}"
	})
	return out, names
}

func (g *schemaGen) operation(d routeDef, pathNames []string) obj {
	desc := []string{}
	switch {
	case strings.HasPrefix(d.ID, "B-P"):
		desc = append(desc, "Permission: `"+string(domain.TenantRoutePermission[d.ID])+"` (§5.2). x-tenant-id must equal the path tenant.")
	case strings.HasPrefix(d.ID, "B-O"):
		p := "`platform_operator`"
		if d.ID == "B-O17" {
			p += " and `pci_owner_reviewer`; the reviewer must not be the submitter"
		}
		desc = append(desc, "Requires "+p+" (BIL-AUTH-2, BIL-AUTH-6).")
	case d.ID == "B-I1":
		desc = append(desc, "mTLS principal cluster.local/ns/bff/sa/bff-signup only (BIL-AUTH-3); Idempotency-Key must be charge:<tenant_id>.")
	}
	params := []any{}
	for _, n := range pathNames {
		params = append(params, obj{"name": n, "in": "path", "required": true, "schema": pathSchema(n)})
	}
	if d.Idem != idemNone {
		params = append(params, obj{"name": headerIdempotencyKey, "in": "header", "required": d.Idem == idemRequired,
			"schema": obj{"type": "string", "pattern": idempotencyKeyPattern.String()}})
	}
	if q, ok := queryTypes[d.ID]; ok {
		params = append(params, g.queryParams(q)...)
	}
	op := obj{"operationId": d.ID, "summary": d.ID + " — " + summaries[d.ID], "description": strings.Join(desc, " "),
		"tags": []string{tagFor(d.ID)}, "parameters": params}
	if d.handler.req != nil {
		op["requestBody"] = obj{"required": true, "content": obj{"application/json": obj{"schema": g.schema(d.handler.req)}}}
	}
	ok := obj{"description": http.StatusText(d.handler.status)}
	switch {
	case d.handler.status == http.StatusFound:
		ok["headers"] = obj{"Location": obj{"schema": obj{"type": "string", "format": "uri"}}}
	case d.handler.res != nil:
		ok["content"] = obj{"application/json": obj{"schema": g.schema(d.handler.res)}}
	}
	responses := obj{strconv.Itoa(d.handler.status): ok,
		"default": obj{"description": "RFC 9457 problem (§17)", "content": obj{"application/problem+json": obj{"schema": obj{"$ref": "#/components/schemas/Problem"}}}}}
	if d.Idem != idemNone && d.handler.status != http.StatusOK {
		responses["200"] = obj{"description": "Idempotent replay (Idempotent-Replayed: true)"}
	}
	op["responses"] = responses
	return op
}

func tagFor(id string) string {
	switch {
	case strings.HasPrefix(id, "B-P"):
		return "tenant"
	case strings.HasPrefix(id, "B-O"):
		return "operator"
	}
	return "internal"
}

func pathSchema(name string) obj {
	switch name {
	case "provider":
		return obj{"type": "string", "enum": []string{"stripe", "manual"}}
	case "provider_event_id":
		return obj{"type": "string", "maxLength": 255}
	}
	return obj{"type": "string", "format": "uuid"}
}

func (g *schemaGen) queryParams(t reflect.Type) []any {
	var out []any
	for _, f := range fields(t) {
		name := strings.Split(f.Tag.Get("form"), ",")[0]
		if name == "" {
			continue
		}
		s := g.schema(f.Type)
		applyBinding(s, f.Tag.Get("binding"))
		out = append(out, obj{"name": name, "in": "query", "required": false, "schema": s})
	}
	return out
}

// ── JSON Schema from Go types ──────────────────────────────────────────────

type schemaGen struct{ defs obj }

var (
	timeType = reflect.TypeFor[time.Time]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

func (g *schemaGen) schema(t reflect.Type) obj {
	switch t {
	case timeType:
		return obj{"type": "string", "format": "date-time"}
	case rawType:
		return obj{}
	}
	switch t.Kind() {
	case reflect.Pointer:
		s := g.schema(t.Elem())
		if _, isRef := s["$ref"]; isRef {
			return obj{"anyOf": []any{s, obj{"type": "null"}}}
		}
		if typ, ok := s["type"].(string); ok {
			s["type"] = []string{typ, "null"}
		}
		return s
	case reflect.String:
		return obj{"type": "string"}
	case reflect.Bool:
		return obj{"type": "boolean"}
	case reflect.Int, reflect.Int32, reflect.Int64:
		return obj{"type": "integer"}
	case reflect.Float64:
		return obj{"type": "number"}
	case reflect.Slice:
		return obj{"type": "array", "items": g.schema(t.Elem())}
	case reflect.Map:
		return obj{"type": "object", "additionalProperties": g.schema(t.Elem())}
	case reflect.Interface:
		return obj{}
	case reflect.Struct:
		name := t.Name()
		if name == "" || strings.Contains(name, "[") { // anonymous or generic: inline
			return g.object(t, name)
		}
		if _, done := g.defs[name]; !done {
			g.defs[name] = obj{} // break recursion
			g.defs[name] = g.object(t, name)
		}
		return obj{"$ref": "#/components/schemas/" + name}
	default:
		return obj{}
	}
}

func (g *schemaGen) object(t reflect.Type, _ string) obj {
	props, required := obj{}, []string{}
	for _, f := range fields(t) {
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" || name == "" {
			continue
		}
		s := g.schema(f.Type)
		b := f.Tag.Get("binding")
		if _, isRef := s["$ref"]; !isRef {
			applyBinding(s, b)
		}
		props[name] = s
		if strings.Contains(","+b+",", ",required,") {
			required = append(required, name)
		}
	}
	out := obj{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		out["required"] = required
	}
	return out
}

// fields lists t's JSON fields, flattening embedded structs.
func fields(t reflect.Type) []reflect.StructField {
	var out []reflect.StructField
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous && f.Tag.Get("json") == "" && f.Type.Kind() == reflect.Struct {
			out = append(out, fields(f.Type)...)
			continue
		}
		if f.IsExported() {
			out = append(out, f)
		}
	}
	return out
}

// applyBinding maps the validator tags onto JSON Schema keywords.
func applyBinding(s obj, tag string) {
	typ, _ := s["type"].(string)
	if l, ok := s["type"].([]string); ok {
		typ = l[0]
	}
	for _, rule := range strings.Split(tag, ",") {
		k, v, _ := strings.Cut(rule, "=")
		switch k {
		case "oneof":
			s["enum"] = strings.Fields(v)
		case "min", "max":
			n, err := strconv.Atoi(v)
			if err != nil {
				continue
			}
			key := map[string]map[string]string{"string": {"min": "minLength", "max": "maxLength"},
				"array": {"min": "minItems", "max": "maxItems"}}[typ][k]
			if key == "" {
				key = map[string]string{"min": "minimum", "max": "maximum"}[k]
			}
			s[key] = n
		case "uuid":
			s["format"] = "uuid"
		case "email":
			s["format"] = "email"
		case "iso4217":
			s["pattern"] = "^[A-Z]{3}$"
		case "iso3166_1_alpha2":
			s["pattern"] = "^[A-Z]{2}$"
		case "taxid":
			s["pattern"] = taxIDPattern.String()
		}
	}
}

func (g *schemaGen) withProblem() obj {
	g.defs["Problem"] = obj{"type": "object", "required": []string{"type", "title", "status", "errorCode"},
		"properties": obj{
			"type": obj{"type": "string", "format": "uri"}, "title": obj{"type": "string"}, "status": obj{"type": "integer"},
			"detail": obj{"type": "string"}, "instance": obj{"type": "string"}, "errorCode": obj{"type": "string"},
			"validationErrors": obj{"type": "array", "items": obj{"type": "object", "properties": obj{
				"field": obj{"type": "string"}, "message": obj{"type": "string"}}}},
			"traceId": obj{"type": "string"}, "tenantId": obj{"type": "string"}, "timestamp": obj{"type": "string", "format": "date-time"},
		}, "additionalProperties": true}
	return g.defs
}

// DocsRoutes serves the API documents: GET /openapi.json (this document),
// GET /asyncapi and GET /asyncapi.yaml (§7.6). They carry no tenant data.
func DocsRoutes(asyncAPI []byte) func(gin.IRouter) {
	openAPI := OpenAPI()
	return func(r gin.IRouter) {
		r.GET("/openapi.json", func(c *gin.Context) { c.Data(http.StatusOK, "application/json", openAPI) })
		yaml := func(c *gin.Context) { c.Data(http.StatusOK, "application/yaml", asyncAPI) }
		r.GET("/asyncapi", yaml)
		r.GET("/asyncapi.yaml", yaml)
	}
}
