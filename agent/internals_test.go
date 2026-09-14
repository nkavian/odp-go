package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	odp "github.com/offering-protocol/odp-go"
	"github.com/offering-protocol/odp-go/directory"
)

const capabilityDocument = `{"description":"Catalog","http":{"endpoint_base":"/odp"},"language":"en","localizations":["en"],"name":"Example","odp_version":"1.0","operations":[{"authentication":"not-required","name":"get-collection"},{"authentication":"not-required","name":"get-offering"},{"authentication":"not-required","name":"list-offerings"},{"authentication":"not-required","name":"search-offerings"}],"search_capabilities":%s}`

func filterJSON(id string) string {
	return fmt.Sprintf(`{"description":"Filter %s","id":"%s","operators":["eq"],"title":"Filter %s","type":"string"}`, id, id, id)
}

func sortJSON(id, filterID string) string {
	return fmt.Sprintf(`{"description":"Sort %s","id":"%s","keys":[{"direction":"ascending","filter_id":"%s","missing":"last"}],"title":"Sort %s"}`, id, id, filterID, id)
}

// capabilityService serves a Service Document with the given search_capabilities plus whatever
// capability pages the handler provides.
func capabilityService(t *testing.T, capabilities string, pages func(*http.Request) (string, bool)) *ServiceClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", mediaTypeODP)
		if request.URL.Path == "/.well-known/odp" {
			fmt.Fprintf(writer, capabilityDocument, capabilities)
			return
		}
		if pages != nil {
			if body, ok := pages(request); ok {
				fmt.Fprint(writer, body)
				return
			}
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	client, err := NewServiceClient(ServiceClientOptions{AllowLocalNetwork: true, ServiceURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestLinkedCapabilitySourcesArePagedAndBounded(t *testing.T) {
	capabilities := `{"filters":{"linked":{"href":"/odp/filters"}},"sorts":{"linked":{"href":"/odp/sorts"}}}`
	client := capabilityService(t, capabilities, func(request *http.Request) (string, bool) {
		switch {
		case request.URL.Path == "/odp/filters" && request.URL.Query().Get("page") == "":
			return `{"odp_version":"1.0","items":[` + filterJSON("region") + `],"next":"/odp/filters?page=2"}`, true
		case request.URL.Path == "/odp/filters":
			return `{"odp_version":"1.0","items":[` + filterJSON("memory") + `]}`, true
		case request.URL.Path == "/odp/sorts":
			return `{"odp_version":"1.0","items":[` + sortJSON("by-region", "region") + `]}`, true
		}
		return "", false
	})
	catalog, err := client.GetOfferingSearchCapabilities(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Filters) != 2 || len(catalog.Sorts) != 1 || len(catalog.Issues) != 0 {
		t.Fatalf("catalog = %#v", catalog)
	}
	if len(catalog.Sorts["by-region"].Filters) != 1 {
		t.Fatalf("resolved sort filters = %#v", catalog.Sorts["by-region"])
	}
}

func TestLinkedCapabilitySourceStaysOnTheServiceOrigin(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", mediaTypeODP)
		fmt.Fprint(writer, `{"odp_version":"1.0","items":[`+filterJSON("region")+`]}`)
	}))
	defer foreign.Close()
	// The request that retrieves a capability source carries this client's credentials, so a
	// document that links off-origin must not be followed.
	capabilities := fmt.Sprintf(`{"filters":{"linked":{"href":"%s/filters"}}}`, foreign.URL)
	client := capabilityService(t, capabilities, nil)
	catalog, err := client.GetOfferingSearchCapabilities(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Filters) != 0 || len(catalog.Issues) != 1 {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestLinkedCapabilitySourceRefusesLoopsAndOverlongChains(t *testing.T) {
	looping := capabilityService(t, `{"filters":{"linked":{"href":"/odp/filters"}}}`, func(*http.Request) (string, bool) {
		return `{"odp_version":"1.0","items":[` + filterJSON("region") + `],"next":"/odp/filters"}`, true
	})
	catalog, err := looping.GetOfferingSearchCapabilities(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Issues) != 1 || !strings.Contains(catalog.Issues[0].Message, "loop") {
		t.Fatalf("loop issues = %#v", catalog.Issues)
	}

	page := 0
	endless := capabilityService(t, `{"filters":{"linked":{"href":"/odp/filters?page=0"}}}`, func(*http.Request) (string, bool) {
		page++
		return fmt.Sprintf(`{"odp_version":"1.0","items":[%s],"next":"/odp/filters?page=%d"}`, filterJSON(fmt.Sprintf("f%d", page)), page), true
	})
	catalog, err = endless.GetOfferingSearchCapabilities(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Issues) != 1 || !strings.Contains(catalog.Issues[0].Message, "16 pages") {
		t.Fatalf("page-limit issues = %#v", catalog.Issues)
	}
}

func TestLinkedCapabilitySourceStopsAtTheEffectiveBound(t *testing.T) {
	// FLT-58: once a source cannot fit the effective bound, retrieval stops rather than buffering
	// every page and discarding the lot at the end.
	requested := 0
	definitions := make([]string, 100)
	for index := range definitions {
		definitions[index] = filterJSON(fmt.Sprintf("f%d-%%d", index))
	}
	client := capabilityService(t, `{"filters":{"linked":{"href":"/odp/filters?page=0"}}}`, func(*http.Request) (string, bool) {
		requested++
		items := make([]string, 100)
		for index := range items {
			items[index] = filterJSON(fmt.Sprintf("f%d-%d", requested, index))
		}
		return fmt.Sprintf(`{"odp_version":"1.0","items":[%s],"next":"/odp/filters?page=%d"}`, strings.Join(items, ","), requested), true
	})
	catalog, err := client.GetOfferingSearchCapabilities(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Filters) != 0 || len(catalog.Issues) != 1 {
		t.Fatalf("catalog = %#v", catalog)
	}
	if requested > 11 {
		t.Fatalf("pages retrieved = %d, want the source abandoned once it could not fit", requested)
	}
}

func TestCapabilitySourcesAreAtomicAndQuarantineAcrossScopes(t *testing.T) {
	// FLT-55: a source that repeats an identifier within itself is not usable at all.
	repeated := capabilityService(t, `{"filters":{"inline":[`+filterJSON("region")+`,`+filterJSON("region")+`]}}`, nil)
	catalog, err := repeated.GetOfferingSearchCapabilities(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Filters) != 0 || len(catalog.Issues) != 1 || !strings.Contains(catalog.Issues[0].Message, "repeats filter region") {
		t.Fatalf("intra-source duplicate = %#v", catalog)
	}

	repeatedSorts := capabilityService(t, `{"filters":{"inline":[`+filterJSON("region")+`]},"sorts":{"inline":[`+sortJSON("by-region", "region")+`,`+sortJSON("by-region", "region")+`]}}`, nil)
	catalog, err = repeatedSorts.GetOfferingSearchCapabilities(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Sorts) != 0 || len(catalog.Issues) != 1 || !strings.Contains(catalog.Issues[0].Message, "repeats sort by-region") {
		t.Fatalf("intra-source duplicate sorts = %#v", catalog)
	}

	// FLT-65: an identifier two scopes both publish is withdrawn from both.
	crossScope := capabilityService(t, `{"filters":{"inline":[`+filterJSON("region")+`]}}`, nil)
	collection := &odp.Collection{SearchCapabilities: &odp.SearchCapabilities{
		Filters: &odp.FilterCapabilitySource{Inline: []odp.FilterDefinition{
			{Description: "Region", ID: "region", Operators: []odp.FilterOperator{odp.OperatorEqual}, Title: "Region", Type: odp.FilterString},
		}},
	}}
	catalog, err = crossScope.resolveSearchCapabilities(t.Context(), collection)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Filters) != 0 || len(catalog.Issues) != 1 || !strings.Contains(catalog.Issues[0].Message, "Duplicate filters: region") {
		t.Fatalf("cross-scope duplicate = %#v", catalog)
	}
}

func TestSortsReferencingAnUnavailableFilterAreReportedInOrder(t *testing.T) {
	sorts := []string{sortJSON("by-c", "missing-c"), sortJSON("by-a", "missing-a"), sortJSON("by-b", "missing-b")}
	client := capabilityService(t, `{"filters":{"inline":[`+filterJSON("region")+`]},"sorts":{"inline":[`+strings.Join(sorts, ",")+`]}}`, nil)
	// Issues reach the caller, so their order cannot depend on Go's map iteration.
	for attempt := 0; attempt < 8; attempt++ {
		catalog, err := client.GetOfferingSearchCapabilities(t.Context(), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(catalog.Issues) != 3 {
			t.Fatalf("issues = %#v", catalog.Issues)
		}
		want := []string{"Sort by-a", "Sort by-b", "Sort by-c"}
		for index, issue := range catalog.Issues {
			if !strings.HasPrefix(issue.Message, want[index]) {
				t.Fatalf("issue %d = %q, want %q", index, issue.Message, want[index])
			}
		}
	}
}

func TestCollectionScopedCapabilitiesRequireTheSearchOperation(t *testing.T) {
	document := `{"description":"Catalog","http":{"endpoint_base":"/odp"},"language":"en","localizations":["en"],"name":"Example","odp_version":"1.0","operations":[{"authentication":"not-required","name":"get-collection"},{"authentication":"not-required","name":"get-offering"},{"authentication":"not-required","name":"list-offerings"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", mediaTypeODP)
		if request.URL.Path == "/.well-known/odp" {
			fmt.Fprint(writer, document)
			return
		}
		fmt.Fprint(writer, `{"odp_version":"1.0","id":"compute","name":"Compute","search_capabilities":{"filters":{"inline":[`+filterJSON("region")+`]}}}`)
	}))
	defer server.Close()
	client, err := NewServiceClient(ServiceClientOptions{AllowLocalNetwork: true, ServiceURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := client.GetCollectionSearchCapabilities(t.Context(), "compute")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Filters) != 0 || len(catalog.Issues) != 1 || catalog.Issues[0].Scope != CapabilityScopeCollection {
		t.Fatalf("catalog = %#v", catalog)
	}
	if _, err := client.GetCollectionSearchCapabilities(t.Context(), "!!"); err == nil {
		t.Fatal("malformed Collection identifier accepted")
	}
}

func TestSupportingDocumentsStayOnTheirOrigin(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/schema" {
			writer.Header().Set("Location", "https://elsewhere.example/schema")
			writer.WriteHeader(http.StatusFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"value":"local"}`)
	}))
	defer origin.Close()
	client, err := NewServiceClient(ServiceClientOptions{
		AllowLocalNetwork: true, ServiceURL: "https://service.example", SupportingHTTPClient: origin.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A redirect cannot repoint a schema or OpenAPI URL at another origin after the fact.
	_, err = client.supportingJSON(t.Context(), origin.URL+"/schema", "test", "application/json",
		[]string{"application/json"}, 1024, maximumResourceDepth, 0)
	if err == nil || !strings.Contains(err.Error(), "changed origin") {
		t.Fatalf("cross-origin supporting redirect error = %v", err)
	}
	// A same-origin redirect is still followed.
	document, err := client.supportingJSON(t.Context(), origin.URL+"/document", "test", "application/json",
		[]string{"application/json"}, 1024, maximumResourceDepth, 0)
	if err != nil || document["value"] != "local" {
		t.Fatalf("same-origin document = %#v, %v", document, err)
	}
}

func TestSupportingDocumentLimitsAndShapes(t *testing.T) {
	cases := map[string]struct {
		body        string
		contentType string
		depth       int
		limit       int64
		wantErr     string
	}{
		"oversized":        {body: `{"v":"` + strings.Repeat("x", 200) + `"}`, contentType: "application/json", depth: 16, limit: 32, wantErr: "byte limit"},
		"too deep":         {body: strings.Repeat(`{"a":`, 20) + `1` + strings.Repeat("}", 20), contentType: "application/json", depth: 8, limit: 4096, wantErr: "nesting-depth"},
		"wrong media type": {body: `{"v":1}`, contentType: "text/plain", depth: 16, limit: 4096, wantErr: "unsupported media type"},
		"not an object":    {body: `[1,2]`, contentType: "application/json", depth: 16, limit: 4096, wantErr: "must be a JSON object"},
		"trailing value":   {body: `{"v":1} {"v":2}`, contentType: "application/json", depth: 16, limit: 4096, wantErr: "one JSON value"},
	}
	for name, test := range cases {
		server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", test.contentType)
			fmt.Fprint(writer, test.body)
		}))
		client, err := NewServiceClient(ServiceClientOptions{
			AllowLocalNetwork: true, ServiceURL: "https://service.example", SupportingHTTPClient: server.Client(),
		})
		if err != nil {
			t.Fatal(err)
		}
		target := server.URL + "/document"
		_, err = client.supportingJSON(t.Context(), target, "test", "application/json", []string{"application/json"}, test.limit, test.depth, 0)
		if err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Errorf("%s: error = %v, want %q", name, err, test.wantErr)
		}
		server.Close()
	}
}

func TestSupportingDocumentsRevalidateAndRecoverFromAForeignEntry(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("ETag", `"v1"`)
		if request.Header.Get("If-None-Match") == `"v1"` {
			writer.WriteHeader(http.StatusNotModified)
			return
		}
		writer.Header().Set("Cache-Control", "max-age=0")
		fmt.Fprint(writer, `{"value":1}`)
	}))
	defer server.Close()
	cache := NewMemoryCache()
	client, err := NewServiceClient(ServiceClientOptions{
		AllowLocalNetwork: true, Cache: cache, ServiceURL: "https://service.example", SupportingHTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	target := server.URL + "/document"
	for attempt := 0; attempt < 2; attempt++ {
		document, err := client.supportingJSON(t.Context(), target, "test", "application/json", []string{"application/json"}, 4096, maximumResourceDepth, time.Hour)
		if err != nil || document["value"] == nil {
			t.Fatalf("attempt %d = %#v, %v", attempt, document, err)
		}
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want a fetch and a revalidation", requests)
	}

	// An entry whose stored representation came from elsewhere is not this document.
	key := cacheKey("anonymous:test:application/json", http.MethodGet, target, "", nil)
	record, _, err := cache.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	record.FinalURL = "https://elsewhere.example/document"
	record.ExpiresAt = time.Now().Add(time.Hour)
	if err := cache.Set(t.Context(), key, record); err != nil {
		t.Fatal(err)
	}
	if _, err := client.supportingJSON(t.Context(), target, "test", "application/json", []string{"application/json"}, 4096, maximumResourceDepth, time.Hour); err != nil {
		t.Fatal(err)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want the foreign entry discarded and refetched", requests)
	}
}

func TestDestinationPolicyRejectsNonPublicAddresses(t *testing.T) {
	// Each IPv6 transition range embeds an IPv4 address, so a name resolving into one of them
	// reaches loopback or link-local metadata without ever naming it.
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.0.0.0",
		"100.64.0.1", "192.0.2.1", "198.18.0.1", "203.0.113.1", "240.0.0.1", "192.175.48.1",
		"::1", "fe80::1", "fc00::1", "2001:db8::1", "64:ff9b::a9fe:a9fe", "64:ff9b::7f00:1",
		"64:ff9b:1::7f00:1", "2002:7f00:1::", "2001::1", "2001:2::1", "2001:10::1", "2001:20::1",
		"::7f00:1", "fec0::1", "5f00::1", "2620:4f:8000::1", "ff02::1", "::ffff:127.0.0.1",
	}
	for _, address := range blocked {
		if isPublicAddress(netip.MustParseAddr(address)) {
			t.Errorf("%s classified as public", address)
		}
	}
	for _, address := range []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946", "8.8.8.8"} {
		if !isPublicAddress(netip.MustParseAddr(address)) {
			t.Errorf("%s classified as non-public", address)
		}
	}
	for _, host := range []string{"localhost", "LOCALHOST", "127.0.0.1", "::1"} {
		if !isLocalDevelopmentHost(host) {
			t.Errorf("%s is not recognised as a development host", host)
		}
	}
	if isLocalDevelopmentHost("service.example") {
		t.Error("public host recognised as a development host")
	}
}

func TestSecureClientRefusesNonPublicDestinations(t *testing.T) {
	client := secureHTTPClient(false)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(request); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("loopback destination error = %v", err)
	}
	unresolvable, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://invalid.invalid./", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(unresolvable); err == nil {
		t.Fatal("unresolvable host accepted")
	}
}

func TestCacheDirectivesAndFreshness(t *testing.T) {
	header := func(values map[string]string) http.Header {
		result := http.Header{}
		for name, value := range values {
			result.Set(name, value)
		}
		return result
	}
	now := time.Unix(1_700_000_000, 0)

	if got := expiry(header(map[string]string{"Cache-Control": "no-store"}), time.Hour, now); !got.Equal(now) {
		t.Errorf("no-store expiry = %v", got)
	}
	if got := expiry(header(map[string]string{"Cache-Control": "no-cache"}), time.Hour, now); !got.Equal(now) {
		t.Errorf("no-cache expiry = %v", got)
	}
	if got := expiry(header(map[string]string{"Cache-Control": "max-age=60", "Age": "20"}), time.Hour, now); !got.Equal(now.Add(40 * time.Second)) {
		t.Errorf("aged max-age expiry = %v", got)
	}
	if got := expiry(header(map[string]string{"Cache-Control": "max-age=abc"}), time.Minute, now); !got.Equal(now.Add(time.Minute)) {
		t.Errorf("unreadable max-age expiry = %v", got)
	}
	expires := now.Add(2 * time.Hour).UTC().Format(http.TimeFormat)
	if got := expiry(header(map[string]string{"Expires": expires}), time.Minute, now); got.IsZero() {
		t.Errorf("Expires expiry = %v", got)
	}
	if got := expiry(http.Header{}, time.Minute, now); !got.Equal(now.Add(time.Minute)) {
		t.Errorf("fallback expiry = %v", got)
	}

	if hasFreshnessDirective(http.Header{}) {
		t.Error("bare header reported a freshness directive")
	}
	if !hasFreshnessDirective(header(map[string]string{"Expires": expires})) {
		t.Error("Expires is a freshness directive")
	}
	if !explicitFreshness(header(map[string]string{"Cache-Control": "max-age=5"})) {
		t.Error("max-age is explicit freshness")
	}

	if directives := cacheDirectives(`max-age="30", , no-cache`); directives["max-age"] != "30" {
		t.Errorf("directives = %#v", directives)
	}
	if supportedVary([]string{"Accept, Accept-Language"}) != true || supportedVary([]string{"X-Tenant"}) != false {
		t.Error("Vary support classification is wrong")
	}
}

func TestCacheabilityByMethodAndDirective(t *testing.T) {
	cases := []struct {
		fallback time.Duration
		header   http.Header
		method   string
		want     bool
	}{
		{fallback: time.Hour, header: http.Header{}, method: http.MethodGet, want: true},
		{fallback: 0, header: http.Header{}, method: http.MethodGet, want: false},
		{fallback: 0, header: http.Header{"Cache-Control": []string{"no-cache"}}, method: http.MethodGet, want: true},
		{fallback: 0, header: http.Header{"Cache-Control": []string{"max-age=60"}}, method: http.MethodPost, want: true},
		{fallback: time.Hour, header: http.Header{}, method: http.MethodPost, want: false},
		{fallback: time.Hour, header: http.Header{"Cache-Control": []string{"no-store"}}, method: http.MethodGet, want: false},
		{fallback: time.Hour, header: http.Header{"Vary": []string{"X-Tenant"}}, method: http.MethodGet, want: false},
		{fallback: -time.Second, header: http.Header{}, method: http.MethodGet, want: false},
		{fallback: time.Hour, header: http.Header{}, method: http.MethodDelete, want: false},
	}
	for index, test := range cases {
		if got := cacheable(test.method, test.header, test.fallback); got != test.want {
			t.Errorf("case %d: cacheable = %v, want %v", index, got, test.want)
		}
	}
}

func TestCacheKeyAndRecordHelpers(t *testing.T) {
	if cacheKey("a", "GET", "https://x/", "en", nil) == cacheKey("b", "GET", "https://x/", "en", nil) {
		t.Error("partitions share a cache key")
	}
	if cacheKey("a", "POST", "https://x/", "", []byte(`{"q":1}`)) == cacheKey("a", "POST", "https://x/", "", []byte(`{"q":2}`)) {
		t.Error("request bodies share a cache key")
	}
	if _, cached, err := cachedRecord(t.Context(), nil, "key"); cached || err != nil {
		t.Errorf("absent cache = %v, %v", cached, err)
	}
	if _, cached, err := cachedRecord(t.Context(), NewMemoryCache(), ""); cached || err != nil {
		t.Errorf("absent key = %v, %v", cached, err)
	}
	for _, status := range []int{301, 302, 303, 307, 308} {
		if !redirectStatus(status) {
			t.Errorf("%d is a redirect", status)
		}
	}
	if redirectStatus(http.StatusOK) {
		t.Error("200 is not a redirect")
	}
}

func TestSchemaWalkersRejectUnsupportedConstructs(t *testing.T) {
	if err := requireSupportedVocabularies(map[string]any{"$vocabulary": map[string]any{"https://vendor.example/v": true}}); err == nil {
		t.Error("unsupported vocabulary accepted")
	}
	if err := requireSupportedVocabularies(map[string]any{"$vocabulary": map[string]any{"https://vendor.example/v": false}}); err != nil {
		t.Errorf("optional vocabulary rejected: %v", err)
	}
	if err := requireSupportedVocabularies([]any{map[string]any{"$vocabulary": map[string]any{"https://vendor.example/v": true}}}); err == nil {
		t.Error("unsupported vocabulary in an array accepted")
	}
	if err := requireFragmentDynamicReferences([]any{map[string]any{"$dynamicRef": "https://other.example/#node"}}); err == nil {
		t.Error("external dynamic reference accepted")
	}
	if err := requireFragmentDynamicReferences(map[string]any{"$dynamicRef": 1}); err == nil {
		t.Error("non-string dynamic reference accepted")
	}
	if err := requireFragmentDynamicReferences(map[string]any{"$dynamicRef": "#node"}); err != nil {
		t.Errorf("fragment dynamic reference rejected: %v", err)
	}
	if _, err := resolveSchemaReference("http://insecure.example/s.json", "https://schema.example/"); err == nil {
		t.Error("insecure schema reference accepted")
	}
	if _, err := resolveSchemaReference("", "://"); err == nil {
		t.Error("unparseable base accepted")
	}
	references, err := schemaReferences(map[string]any{
		"$id":        "https://schema.example/root.json",
		"properties": map[string]any{"a": map[string]any{"$ref": "https://schema.example/leaf.json"}, "b": map[string]any{"$ref": "#/$defs/local"}},
	}, "https://schema.example/root.json")
	if err != nil || len(references) != 1 || references[0] != "https://schema.example/leaf.json" {
		t.Fatalf("references = %#v, %v", references, err)
	}
}

func TestActionTargetResolution(t *testing.T) {
	if _, err := resolveHTTPReference("://", "https://service.example"); err == nil {
		t.Error("unparseable Action target accepted")
	}
	if _, err := resolveHTTPReference("mailto:someone@example.com", "https://service.example"); err == nil {
		t.Error("non-HTTP Action target accepted")
	}
	if _, err := resolveHTTPSReference("http://localhost/schema.json", "https://service.example"); err == nil {
		t.Error("insecure supporting document accepted")
	}
	target, err := resolveHTTPReference("/buy", "https://service.example")
	if err != nil || target != "https://service.example/buy" {
		t.Fatalf("resolved target = %q, %v", target, err)
	}
	if _, err := resolveHTTPSReference("://", "https://service.example"); err == nil {
		t.Error("unparseable supporting document accepted")
	}
}

func TestActionsWithoutAUsableTargetAreReported(t *testing.T) {
	actions := []odp.Action{
		{Authentication: odp.AuthenticationNotRequired, ID: "orphan", Rel: odp.ActionPurchase},
		{Authentication: odp.AuthenticationNotRequired, HTTP: &odp.HTTPActionTarget{Href: "/buy", Method: http.MethodPost}, ID: "buy", Rel: odp.ActionPurchase},
	}
	discovered, issues := normalizeActions(actions, "https://service.example", "")
	if len(discovered) != 1 || discovered[0].ID != "buy" {
		t.Fatalf("actions = %#v", discovered)
	}
	// A dropped Action that produced no issue is indistinguishable from one the Service never sent.
	if len(issues) != 1 || issues[0].ActionID != "orphan" {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestPureHelpersCoverTheirBranches(t *testing.T) {
	if defaultRepresentation("", odp.RepresentationFull) != odp.RepresentationFull {
		t.Error("empty representation did not fall back")
	}
	if defaultRepresentation(odp.RepresentationTerse, odp.RepresentationFull) != odp.RepresentationTerse {
		t.Error("explicit representation was overridden")
	}
	if requestLimit(0, 25) != 25 || requestLimit(7, 25) != 7 {
		t.Error("request limit fallback is wrong")
	}
	document := odp.ServiceDocument{Operations: []odp.OperationDescriptor{{Name: odp.OperationListOfferings}}}
	if !supports(document, odp.OperationListOfferings) || supports(document, odp.OperationSearchOfferings) {
		t.Error("operation support is wrong")
	}
	if _, err := odpMethod(odp.Operation("not-an-operation")); !errors.Is(err, ErrUnsupportedOperation) {
		t.Errorf("unknown operation method error = %v", err)
	}
	if method, err := odpMethod(odp.OperationSearchOfferings); err != nil || method != http.MethodPost {
		t.Errorf("search method = %q, %v", method, err)
	}
	if repeated, found := firstRepeated([]string{"a", "b", "a"}); !found || repeated != "a" {
		t.Errorf("firstRepeated = %q, %v", repeated, found)
	}
	if _, found := firstRepeated([]string{"a", "b"}); found {
		t.Error("distinct identifiers reported as repeated")
	}
	if names := duplicateNames(map[string]bool{"b": true, "a": true}); names != "a, b" {
		t.Errorf("duplicateNames = %q", names)
	}
	if keys := sortedKeys(map[string]int{"b": 1, "a": 1}); strings.Join(keys, ",") != "a,b" {
		t.Errorf("sortedKeys = %#v", keys)
	}
	if err := requireOfferingRepresentation(odp.Offering{DetailFields: []string{"/a"}}, odp.RepresentationTerse); err != nil {
		t.Errorf("terse detail_fields rejected: %v", err)
	}
	if err := requireCollectionRepresentation(odp.Collection{DetailFields: []string{"/a"}}, odp.RepresentationTerse); err != nil {
		t.Errorf("terse Collection detail_fields rejected: %v", err)
	}
	if err := requireRefinements(nil, refinementPolicy{}); err != nil {
		t.Errorf("absent refinements rejected: %v", err)
	}
	if err := searchRefinementPolicy(nil); err.permitted {
		t.Error("empty refinement request permitted refinements")
	}
	if policy := searchRefinementPolicy([]string{"color"}); !policy.permitted || !policy.allowed["color"] {
		t.Errorf("policy = %#v", policy)
	}
}

func TestFederatedSearchBoundsAreValidated(t *testing.T) {
	cases := map[string]FederatedSearchRequest{
		"too many services":  {MaxServices: 101},
		"negative services":  {MaxServices: -1},
		"too many offerings": {MaxOfferingsPerService: 101},
		"too much work":      {Concurrency: 17},
	}
	instance, err := New(AgentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name, request := range cases {
		var failure error
		for _, err := range instance.SearchOfferingsAcrossServices(context.Background(), request) {
			failure = err
			break
		}
		if failure == nil {
			t.Errorf("%s: bounds accepted", name)
		}
	}
	if instance.Environment() == "" {
		t.Error("agent reported no directory environment")
	}
	if _, err := New(AgentOptions{Environment: directory.Environment("nowhere")}); err == nil {
		t.Error("unknown directory environment accepted")
	}
}
