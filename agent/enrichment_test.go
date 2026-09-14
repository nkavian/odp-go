package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const enrichmentDocument = `{"description":"Catalog","http":{"endpoint_base":"/odp"%s},"language":"en","localizations":["en"],"name":"Example","odp_version":"1.0","operations":[{"authentication":"not-required","name":"get-offering"},{"authentication":"not-required","name":"list-offerings"}]}`

// enrichmentClient serves a catalog over loopback and its supporting documents over TLS, which is
// the shape the protocol requires: ODP resources may use loopback HTTP in development, supporting
// documents must use HTTPS. It returns the client and the origin the documents are served from;
// `{{base}}` in any body is replaced with that origin.
func enrichmentClient(t *testing.T, offering, serviceOpenAPI string, supporting map[string]string) (*ServiceClient, string) {
	t.Helper()
	var base string
	documents := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, found := supporting[request.URL.Path]
		if !found {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		contentType := "application/schema+json"
		if strings.Contains(request.URL.Path, "openapi") {
			contentType = "application/vnd.oai.openapi+json"
		}
		writer.Header().Set("Content-Type", contentType)
		fmt.Fprint(writer, strings.ReplaceAll(body, "{{base}}", base))
	}))
	t.Cleanup(documents.Close)
	base = documents.URL

	openAPI := ""
	if serviceOpenAPI != "" {
		openAPI = fmt.Sprintf(`,"openapi":{"url":"%s%s"}`, base, serviceOpenAPI)
	}
	catalog := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", mediaTypeODP)
		if request.URL.Path == "/.well-known/odp" {
			fmt.Fprintf(writer, enrichmentDocument, openAPI)
			return
		}
		fmt.Fprint(writer, strings.ReplaceAll(offering, "{{base}}", base))
	}))
	t.Cleanup(catalog.Close)

	client, err := NewServiceClient(ServiceClientOptions{
		AllowLocalNetwork: true, ServiceURL: catalog.URL, SupportingHTTPClient: documents.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, base
}

func offeringJSON(members string) string {
	return `{"odp_version":"1.0","id":"gpu","name":"GPU"` + members + `}`
}

const jsonSchemaDialect = `"$schema":"https://json-schema.org/draft/2020-12/schema"`

func TestOfferingDetailsScopeAttributeSchemaFailures(t *testing.T) {
	schema := `{` + jsonSchemaDialect + `,"type":"object","properties":{"memory":{"type":"integer"}},"required":["memory"]}`

	valid, _ := enrichmentClient(t, offeringJSON(`,"schema":{"url":"{{base}}/schema.json"},"attributes":{"memory":80}`), "",
		map[string]string{"/schema.json": schema})
	details, err := valid.GetOfferingDetails(t.Context(), "gpu")
	if err != nil {
		t.Fatal(err)
	}
	if details.AttributeSchema == nil || details.Offering.Attributes == nil || len(details.Issues) != 0 {
		t.Fatalf("valid details = %#v", details)
	}

	// An Offering whose attributes contradict their schema keeps the Offering and drops only the
	// attributes, because the failure is scoped to that enrichment rather than to the resource.
	mismatched, _ := enrichmentClient(t, offeringJSON(`,"schema":{"url":"{{base}}/schema.json"},"attributes":{"memory":"eighty"}`), "",
		map[string]string{"/schema.json": schema})
	details, err = mismatched.GetOfferingDetails(t.Context(), "gpu")
	if err != nil {
		t.Fatal(err)
	}
	if details.Offering.Attributes != nil || len(details.Issues) != 1 || details.Issues[0].Scope != OfferingIssueAttributes {
		t.Fatalf("mismatched details = %#v", details)
	}

	// A schema that cannot be retrieved is the same kind of scoped failure.
	missing, _ := enrichmentClient(t, offeringJSON(`,"schema":{"url":"{{base}}/absent.json"},"attributes":{"memory":80}`), "", nil)
	details, err = missing.GetOfferingDetails(t.Context(), "gpu")
	if err != nil {
		t.Fatal(err)
	}
	if details.Offering.Attributes != nil || len(details.Issues) != 1 || details.Issues[0].Scope != OfferingIssueAttributeSchema {
		t.Fatalf("unretrievable schema details = %#v", details)
	}

	// So is a schema URL that is not a usable supporting-document reference.
	insecure, _ := enrichmentClient(t, offeringJSON(`,"schema":{"url":"/schema.json"},"attributes":{"memory":80}`), "", nil)
	details, err = insecure.GetOfferingDetails(t.Context(), "gpu")
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Issues) != 1 || details.Issues[0].Scope != OfferingIssueAttributeSchema {
		t.Fatalf("insecure schema details = %#v", details)
	}

	// An Offering with a schema but no attributes has nothing to validate.
	bare, _ := enrichmentClient(t, offeringJSON(`,"schema":{"url":"{{base}}/schema.json"}`), "",
		map[string]string{"/schema.json": schema})
	details, err = bare.GetOfferingDetails(t.Context(), "gpu")
	if err != nil || details.AttributeSchema == nil || len(details.Issues) != 0 {
		t.Fatalf("schema without attributes = %#v, %v", details, err)
	}

	// An Offering with no schema at all is returned as it stands.
	plain, _ := enrichmentClient(t, offeringJSON(""), "", nil)
	details, err = plain.GetOfferingDetails(t.Context(), "gpu")
	if err != nil || details.AttributeSchema != nil || len(details.Issues) != 0 {
		t.Fatalf("plain details = %#v, %v", details, err)
	}
	if _, err := plain.GetOfferingDetails(t.Context(), "!!"); err == nil {
		t.Fatal("malformed Offering identifier accepted")
	}
}

const openAPIDocument = `{"openapi":"3.1.0","info":{"title":"Example","version":"1.0.0"},"paths":{"/rent":{"post":{"operationId":"rent","responses":{"200":{"description":"ok"}}}}}}`

func TestResolveActionCoversEveryTargetShape(t *testing.T) {
	requestSchema := `{` + jsonSchemaDialect + `,"type":"object"}`
	actions := `,"actions":[` +
		`{"authentication":"not-required","id":"plain","rel":"purchase","http":{"href":"/buy","method":"POST"}},` +
		`{"authentication":"not-required","id":"schema","rel":"purchase","http":{"href":"/buy","method":"POST","request":{"content_type":"application/json","schema":{"url":"{{base}}/request.json"}}}},` +
		`{"authentication":"not-required","id":"api","rel":"invoke","openapi":{"operation_id":"rent","url":"{{base}}/openapi.json"}}]`

	client, _ := enrichmentClient(t, offeringJSON(actions), "", map[string]string{
		"/request.json": requestSchema, "/openapi.json": openAPIDocument,
	})

	plain, err := client.ResolveAction(t.Context(), "gpu", "plain")
	if err != nil || plain.RequestSchema != nil || plain.Action.HTTP == nil {
		t.Fatalf("plain Action = %#v, %v", plain, err)
	}
	withSchema, err := client.ResolveAction(t.Context(), "gpu", "schema")
	if err != nil || withSchema.RequestSchema == nil {
		t.Fatalf("Action request schema = %#v, %v", withSchema, err)
	}
	api, err := client.ResolveAction(t.Context(), "gpu", "api")
	if err != nil || api.Operation == nil || api.OpenAPIDocument == nil {
		t.Fatalf("OpenAPI Action = %#v, %v", api, err)
	}
	if _, err := client.ResolveAction(t.Context(), "gpu", "absent"); err == nil {
		t.Fatal("unknown Action resolved")
	}
}

func TestResolveActionReportsUnusableSupportingDocuments(t *testing.T) {
	action := func(target string) string {
		return `,"actions":[{"authentication":"not-required","id":"act","rel":"purchase",` + target + `}]`
	}

	// A request schema that is not a usable HTTPS reference fails the resolution outright, unlike
	// an Attribute Schema, which is a scoped enrichment.
	insecure, _ := enrichmentClient(t, offeringJSON(action(`"http":{"href":"/buy","method":"POST","request":{"content_type":"application/json","schema":{"url":"/request.json"}}}`)), "", nil)
	if _, err := insecure.ResolveAction(t.Context(), "gpu", "act"); err == nil {
		t.Fatal("insecure request schema accepted")
	}

	unreachable, _ := enrichmentClient(t, offeringJSON(action(`"openapi":{"operation_id":"rent","url":"{{base}}/absent.json"}`)), "", nil)
	if _, err := unreachable.ResolveAction(t.Context(), "gpu", "act"); err == nil {
		t.Fatal("unreachable OpenAPI document accepted")
	}
}

func TestOpenAPIDocumentsAreValidatedBeforeUse(t *testing.T) {
	cases := map[string]struct {
		document  string
		operation string
		wantErr   string
	}{
		"not 3.1":            {document: `{"openapi":"3.0.3","info":{"title":"E","version":"1"},"paths":{}}`, operation: "rent", wantErr: "OpenAPI 3.1"},
		"missing version":    {document: `{"info":{"title":"E","version":"1"},"paths":{}}`, operation: "rent", wantErr: "OpenAPI 3.1"},
		"invalid model":      {document: `{"openapi":"3.1.0","info":{"title":"E","version":"1.0.0"},"paths":{"/a":{"get":{"responses":{"200":{"$ref":"#/components/absent"}}}}}}`, operation: "rent", wantErr: "document is invalid"},
		"operation missing":  {document: openAPIDocument, operation: "absent", wantErr: "must resolve exactly once"},
		"operation repeated": {document: `{"openapi":"3.1.0","info":{"title":"E","version":"1.0.0"},"paths":{"/a":{"post":{"operationId":"twice","responses":{"200":{"description":"ok"}}}},"/b":{"get":{"operationId":"twice","responses":{"200":{"description":"ok"}}}}}}`, operation: "twice", wantErr: "must resolve exactly once"},
		"no paths":           {document: `{"openapi":"3.1.0","info":{"title":"E","version":"1.0.0"}}`, operation: "rent", wantErr: "must contain paths"},
	}
	for name, test := range cases {
		client, base := enrichmentClient(t, offeringJSON(""), "", map[string]string{"/openapi.json": test.document})
		_, _, err := client.resolveOpenAPI(t.Context(), base+"/openapi.json", test.operation)
		if err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Errorf("%s: error = %v, want %q", name, err, test.wantErr)
		}
	}
}

func TestAttributeSchemaGraphLimits(t *testing.T) {
	documents := map[string]string{}
	// A chain deeper than the eight reference levels the protocol allows.
	for index := 0; index <= 12; index++ {
		documents[fmt.Sprintf("/deep-%d.json", index)] = fmt.Sprintf(
			`{%s,"$id":"{{base}}/deep-%d.json","type":"object","properties":{"next":{"$ref":"{{base}}/deep-%d.json"}}}`,
			jsonSchemaDialect, index, index+1)
	}
	documents["/deep-13.json"] = `{` + jsonSchemaDialect + `,"type":"string"}`
	// A graph wider than sixteen documents.
	wide := make([]string, 0, 20)
	for index := 0; index < 20; index++ {
		documents[fmt.Sprintf("/wide-%d.json", index)] = `{` + jsonSchemaDialect + `,"type":"string"}`
		wide = append(wide, fmt.Sprintf(`"p%d":{"$ref":"{{base}}/wide-%d.json"}`, index, index))
	}
	documents["/wide.json"] = `{` + jsonSchemaDialect + `,"type":"object","properties":{` + strings.Join(wide, ",") + `}}`
	documents["/draft-07.json"] = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object"}`
	documents["/vendor.json"] = `{` + jsonSchemaDialect + `,"$vocabulary":{"https://vendor.example/vocab":true}}`
	documents["/dynamic.json"] = `{` + jsonSchemaDialect + `,"properties":{"a":{"$dynamicRef":"https://other.example/s.json#node"}}}`
	documents["/uncompilable.json"] = `{` + jsonSchemaDialect + `,"type":"object","properties":{"a":{"$ref":"#/$defs/absent"}}}`
	documents["/insecure-ref.json"] = `{` + jsonSchemaDialect + `,"properties":{"a":{"$ref":"http://schema.example/leaf.json"}}}`

	client, base := enrichmentClient(t, offeringJSON(""), "", documents)
	cases := map[string]struct {
		path    string
		wantErr string
	}{
		"too deep":          {path: "/deep-0.json", wantErr: "reference levels"},
		"too wide":          {path: "/wide.json", wantErr: "16 documents"},
		"wrong dialect":     {path: "/draft-07.json", wantErr: "Draft 2020-12"},
		"vendor vocabulary": {path: "/vendor.json", wantErr: "unsupported vocabulary"},
		"external anchor":   {path: "/dynamic.json", wantErr: "fragment-only"},
		"uncompilable":      {path: "/uncompilable.json", wantErr: "compile"},
		"insecure ref":      {path: "/insecure-ref.json", wantErr: "must use HTTPS"},
	}
	for name, test := range cases {
		if _, _, err := client.resolveSchema(t.Context(), base+test.path); err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Errorf("%s: error = %v, want %q", name, err, test.wantErr)
		}
	}
	if _, _, err := client.resolveSchema(t.Context(), "http://schema.example/s.json"); err == nil {
		t.Error("insecure Attribute Schema URL accepted")
	}
	if _, _, err := client.resolveSchema(t.Context(), "://"); err == nil {
		t.Error("unparseable Attribute Schema URL accepted")
	}
}

func TestOfferingDetailsCarryServiceOpenAPIDefaults(t *testing.T) {
	actions := `,"actions":[{"authentication":"not-required","id":"api","rel":"invoke","openapi":{"operation_id":"rent"}}]`
	// An Action that names only an operation inherits the Service Document's OpenAPI URL.
	client, _ := enrichmentClient(t, offeringJSON(actions), "/openapi.json", map[string]string{"/openapi.json": openAPIDocument})
	details, err := client.GetOfferingDetails(t.Context(), "gpu")
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Actions) != 1 || details.Actions[0].OpenAPI == nil || len(details.Issues) != 0 {
		t.Fatalf("details = %#v", details)
	}
	resolved, err := client.ResolveAction(t.Context(), "gpu", "api")
	if err != nil || resolved.Operation == nil {
		t.Fatalf("resolved = %#v, %v", resolved, err)
	}
}

func TestAttributeSchemaDocumentIsRetrievedOncePerGraph(t *testing.T) {
	fetches := map[string]int{}
	documents := map[string]string{
		"/leaf.json": `{` + jsonSchemaDialect + `,"type":"string"}`,
		"/root.json": `{` + jsonSchemaDialect + `,"type":"object","properties":{"a":{"$ref":"{{base}}/leaf.json"},"b":{"$ref":"{{base}}/leaf.json"}}}`,
	}
	var base string
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fetches[request.URL.Path]++
		body, found := documents[request.URL.Path]
		if !found {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/schema+json")
		fmt.Fprint(writer, strings.ReplaceAll(body, "{{base}}", base))
	}))
	defer server.Close()
	base = server.URL
	client, err := NewServiceClient(ServiceClientOptions{
		AllowLocalNetwork: true, ServiceURL: "https://service.example", SupportingHTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.resolveSchema(t.Context(), base+"/root.json"); err != nil {
		t.Fatal(err)
	}
	// Two references to one document are one retrieval, so a schema graph cannot be used to
	// amplify requests against whoever hosts it.
	if fetches["/leaf.json"] != 1 {
		t.Fatalf("leaf retrievals = %d, want 1", fetches["/leaf.json"])
	}
}
