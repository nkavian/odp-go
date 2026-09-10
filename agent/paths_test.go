package agent

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const plainDocument = `{"description":"Catalog","http":{"endpoint_base":"/odp"},"language":"en","localizations":["en"],"name":"Example","odp_version":"1.0","operations":[{"authentication":"not-required","name":"get-offering"},{"authentication":"not-required","name":"list-offerings"}]}`

// catalogCacheClient serves the Service Document from a handler and caches through cache.
func catalogCacheClient(t *testing.T, cache Cache, handler func(*http.Request) (*http.Response, error)) *ServiceClient {
	t.Helper()
	client, err := NewServiceClient(ServiceClientOptions{
		Cache: cache, CachePartition: "test", HTTPClient: &http.Client{Transport: roundTripFunc(handler)}, ServiceURL: "https://service.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// storedRecord returns the single entry a client has cached, so a test can age or corrupt it.
func storedRecord(t *testing.T, cache *countingCache) (string, CacheRecord) {
	t.Helper()
	for key, record := range cache.records {
		return key, record
	}
	t.Fatal("nothing was cached")
	return "", CacheRecord{}
}

// primeDocumentCache answers one request with a storable but immediately stale document, then hands
// every later request to revalidate.
func primeDocumentCache(t *testing.T, cache *countingCache, revalidate func(*http.Request) (*http.Response, error)) *ServiceClient {
	t.Helper()
	served := false
	client := catalogCacheClient(t, cache, func(request *http.Request) (*http.Response, error) {
		if served {
			return revalidate(request)
		}
		served = true
		return &http.Response{
			Body: io.NopCloser(strings.NewReader(plainDocument)),
			Header: http.Header{
				"Cache-Control": {"max-age=0"}, "Content-Type": {mediaTypeODP}, "ETag": {`"v1"`},
			},
			Request: request, StatusCode: http.StatusOK,
		}, nil
	})
	if _, err := client.Inspect(t.Context()); err != nil {
		t.Fatal(err)
	}
	return client
}

func revalidated(headers http.Header) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		if headers == nil {
			headers = http.Header{}
		}
		return &http.Response{Body: io.NopCloser(strings.NewReader("")), Header: headers, StatusCode: http.StatusNotModified}, nil
	}
}

func TestRevalidationDropsAnEntryItCanNoLongerTrust(t *testing.T) {
	// A 304 confirms the stored bytes. When those bytes no longer parse, what is confirmed is not
	// something this Agent can hand back, so the entry goes rather than the caller getting it.
	cache := newCountingCache()
	client := primeDocumentCache(t, cache, revalidated(nil))
	key, record := storedRecord(t, cache)
	record.Body = []byte(`{"odp_version":"1.0"}`)
	cache.records[key] = record
	if _, err := client.Inspect(t.Context()); err == nil {
		t.Fatal("a cached document that no longer validates was served")
	}
	if _, kept := cache.records[key]; kept {
		t.Fatal("the entry survived a failed revalidation")
	}
}

func TestRevalidationCarriesTheLifetimeTheEntryHad(t *testing.T) {
	cache := newCountingCache()
	client := primeDocumentCache(t, cache, revalidated(nil))
	key, record := storedRecord(t, cache)
	// An entry whose expiry precedes the moment it was stored has no lifetime to carry forward.
	record.StoredAt = record.ExpiresAt.Add(time.Minute)
	cache.records[key] = record
	if _, err := client.Inspect(t.Context()); err != nil {
		t.Fatal(err)
	}
	refreshed := cache.records[key]
	if refreshed.ExpiresAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("refreshed expiry = %v", refreshed.ExpiresAt)
	}
}

func TestRevalidationReportsWhatTheCacheCouldNotDo(t *testing.T) {
	for name, test := range map[string]struct {
		headers http.Header
		set     bool
	}{
		// A Vary the cache key does not cover means the stored entry is one variant of several.
		"an entry it could not evict on a new Vary": {headers: http.Header{"Vary": {"User-Agent"}}},
		"an entry it could not evict on no-store":   {headers: http.Header{"Cache-Control": {"no-store"}}},
		"an entry it could not refresh":             {headers: http.Header{}, set: true},
	} {
		cache := newCountingCache()
		client := primeDocumentCache(t, cache, revalidated(test.headers))
		if test.set {
			cache.setErr = fmt.Errorf("cache is unavailable")
		} else {
			cache.deleteErr = fmt.Errorf("cache is unavailable")
		}
		if _, err := client.Inspect(t.Context()); err == nil || !strings.Contains(err.Error(), "cache is unavailable") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestASortTwoScopesBothPublishIsQuarantined(t *testing.T) {
	// FLT-65: an identifier two sources both publish belongs to neither, so it is withdrawn from
	// both rather than one silently winning.
	service := fmt.Sprintf(`{"filters":{"inline":[%s]},"sorts":{"inline":[%s,%s]}}`,
		filterJSON("region"), sortJSON("shared", "region"), sortJSON("only-service", "region"))
	collection := fmt.Sprintf(`{"id":"compute","name":"Compute","odp_version":"1.0","search_capabilities":{"filters":{"inline":[%s]},"sorts":{"inline":[%s]}}}`,
		filterJSON("tier"), sortJSON("shared", "tier"))
	client := capabilityService(t, service, func(request *http.Request) (string, bool) {
		if request.URL.Path == "/odp/collections/compute" {
			return collection, true
		}
		return "", false
	})
	catalog, err := client.GetOfferingSearchCapabilities(t.Context(), "compute")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := catalog.Sorts["shared"]; present {
		t.Fatal("a sort both scopes published was kept")
	}
	if _, present := catalog.Sorts["only-service"]; !present {
		t.Fatalf("sorts = %#v", catalog.Sorts)
	}
	found := false
	for _, issue := range catalog.Issues {
		if issue.Kind == CapabilityKindSorts && strings.Contains(issue.Message, "Duplicate sorts") {
			found = true
		}
	}
	if !found {
		t.Fatalf("issues = %#v", catalog.Issues)
	}
}

func TestCapabilityPagesAreValidatedAsTheyArrive(t *testing.T) {
	deep := strings.Repeat("[", 18) + "0" + strings.Repeat("]", 18)
	for name, test := range map[string]struct {
		body    string
		wantErr string
	}{
		"a page that is not a page":    {body: `{"odp_version":"1.0"}`, wantErr: "page"},
		"a definition that is not one": {body: `{"odp_version":"1.0","items":[{"id":"broken"}]}`, wantErr: "Filter"},
		"a page nested past the limit": {body: `{"odp_version":"1.0","items":[],"x_tower":` + deep + `}`, wantErr: "nesting-depth"},
	} {
		client := capabilityService(t, `{"filters":{"linked":{"href":"/odp/filters"}}}`, func(*http.Request) (string, bool) {
			return test.body, true
		})
		catalog, err := client.GetOfferingSearchCapabilities(t.Context(), "")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(catalog.Issues) != 1 {
			t.Errorf("%s: issues = %#v", name, catalog.Issues)
		}
	}
}

func TestALinkedSortSourceStopsAtItsEffectiveBound(t *testing.T) {
	page := 0
	client := capabilityService(t, `{"filters":{"inline":[`+filterJSON("region")+`]},"sorts":{"linked":{"href":"/odp/sorts?page=0"}}}`,
		func(*http.Request) (string, bool) {
			page++
			items := make([]string, 100)
			for index := range items {
				items[index] = sortJSON(fmt.Sprintf("s%d-%d", page, index), "region")
			}
			return fmt.Sprintf(`{"odp_version":"1.0","items":[%s],"next":"/odp/sorts?page=%d"}`, strings.Join(items, ","), page), true
		})
	catalog, err := client.GetOfferingSearchCapabilities(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Sorts) != 0 || len(catalog.Issues) != 1 {
		t.Fatalf("sorts = %d, issues = %#v", len(catalog.Sorts), catalog.Issues)
	}
	if page > 2 {
		t.Fatalf("pages retrieved = %d, want retrieval to stop at the bound", page)
	}
}

func TestSchemaGraphsShareDocumentsAndKeepTheirOwnDefinitions(t *testing.T) {
	// Two branches referencing one document retrieve it once, and a root that already uses the
	// name the bundler wants keeps its own definition.
	documents := map[string]string{
		"https://schemas.example/root.json":   `{"$schema":"https://json-schema.org/draft/2020-12/schema","$defs":{"odp_external_0":{"type":"string"}},"properties":{"left":{"$ref":"shared.json"},"right":{"$ref":"shared.json"}},"type":"object"}`,
		"https://schemas.example/shared.json": `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"integer"}`,
	}
	requests := map[string]int{}
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests[request.URL.String()]++
		return jsonResponse(request, documents[request.URL.String()], "application/schema+json"), nil
	})}
	client, err := NewServiceClient(ServiceClientOptions{ServiceURL: "https://service.example", SupportingHTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	bundled, _, err := client.resolveSchema(t.Context(), "https://schemas.example/root.json")
	if err != nil {
		t.Fatal(err)
	}
	if requests["https://schemas.example/shared.json"] != 1 {
		t.Fatalf("shared document retrieved %d times", requests["https://schemas.example/shared.json"])
	}
	definitions := bundled["$defs"].(map[string]any)
	if definitions["odp_external_0"].(map[string]any)["type"] != "string" {
		t.Fatalf("the root's own definition was overwritten: %#v", definitions["odp_external_0"])
	}
	if _, renamed := definitions["odp_external_0_"]; !renamed {
		t.Fatalf("definitions = %#v", definitions)
	}
}

func TestNestedVocabulariesAreCheckedToo(t *testing.T) {
	client := supportingTestClient(t, map[string]string{
		"https://schemas.example/root.json": `{"$schema":"https://json-schema.org/draft/2020-12/schema","$defs":{"inner":{"$vocabulary":{"https://example.com/vocab/custom":true}}},"type":"object"}`,
	})
	_, _, err := client.resolveSchema(t.Context(), "https://schemas.example/root.json")
	if err == nil || !strings.Contains(err.Error(), "unsupported vocabulary") {
		t.Fatalf("err = %v", err)
	}
}

func TestNestedSchemaReferencesAreResolvedBeforeTheyAreFollowed(t *testing.T) {
	for name, document := range map[string]string{
		"a nested identifier that is not a URL": `{"$schema":"https://json-schema.org/draft/2020-12/schema","$defs":{"inner":{"$id":"https://[::1/x"}},"type":"object"}`,
		"a nested reference that is not a URL":  `{"$schema":"https://json-schema.org/draft/2020-12/schema","$defs":{"inner":{"$ref":"https://[::1/x"}},"type":"object"}`,
	} {
		client := supportingTestClient(t, map[string]string{"https://schemas.example/root.json": document})
		if _, _, err := client.resolveSchema(t.Context(), "https://schemas.example/root.json"); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
