package agent_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	odp "github.com/offering-protocol/odp-go"
	"github.com/offering-protocol/odp-go/agent"
	"github.com/offering-protocol/odp-go/service"
)

// catalogServer serves a real ODP Service over a static catalog, so navigation is exercised
// against pages a conformant Service actually produces rather than hand-written fixtures.
func catalogServer(t *testing.T) *httptest.Server {
	t.Helper()
	catalog, err := service.NewStaticCatalog(service.StaticCatalogOptions{
		Collections: []odp.Collection{
			{ID: "compute", Name: "Compute", ODPVersion: odp.Version},
			{ID: "storage-tier", Name: "Storage", ODPVersion: odp.Version},
		},
		Offerings: []odp.Offering{
			{CollectionIDs: []string{"compute"}, ID: "gpu", Name: "GPU", ODPVersion: odp.Version},
			{CollectionIDs: []string{"compute"}, ID: "cpu", Name: "CPU", ODPVersion: odp.Version},
			{ID: "disk", Name: "Disk", ODPVersion: odp.Version},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := service.New(service.Options{
		Catalog: catalog,
		Document: odp.ServiceDocument{
			Description: "Example catalog", HTTP: odp.HTTPConfiguration{EndpointBase: "/odp"},
			Language: "en", Localizations: []string{"en"}, Name: "Example",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(runtime)
	t.Cleanup(server.Close)
	return server
}

func catalogClient(t *testing.T, pageSize int) *agent.ServiceClient {
	t.Helper()
	client, err := agent.NewServiceClient(agent.ServiceClientOptions{
		AllowLocalNetwork: true, InitialPageSize: pageSize, ServiceURL: catalogServer(t).URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// collect drains an iterator, returning the values and the first error it reported.
func collect[Value any](sequence func(func(Value, error) bool)) ([]Value, error) {
	var values []Value
	var failure error
	sequence(func(value Value, err error) bool {
		if err != nil {
			failure = err
			return false
		}
		values = append(values, value)
		return true
	})
	return values, failure
}

func TestCollectionNavigationCoversEveryEntryPoint(t *testing.T) {
	client := catalogClient(t, 1)

	items, err := collect(client.ListCollections(t.Context(), agent.ListOptions{}))
	if err != nil || len(items) != 2 {
		t.Fatalf("ListCollections = %d items, %v", len(items), err)
	}
	pages, err := collect(client.ListCollectionPages(t.Context(), agent.ListOptions{}))
	if err != nil || len(pages) != 2 {
		t.Fatalf("ListCollectionPages = %d pages, %v", len(pages), err)
	}
	// A one-item page size means the first page carries a continuation the caller can resume from.
	if pages[0].Next == "" {
		t.Fatal("first Collection page omitted its continuation")
	}
	resumed, err := collect(client.ContinueListCollections(t.Context(), pages[0].Next, agent.ContinuationOptions{}))
	if err != nil || len(resumed) != 1 {
		t.Fatalf("ContinueListCollections = %d items, %v", len(resumed), err)
	}
	resumedPages, err := collect(client.ContinueListCollectionPages(t.Context(), pages[0].Next, agent.ContinuationOptions{}))
	if err != nil || len(resumedPages) != 1 {
		t.Fatalf("ContinueListCollectionPages = %d pages, %v", len(resumedPages), err)
	}
	collection, err := client.GetCollection(t.Context(), "compute", odp.RepresentationFull)
	if err != nil || collection.Name != "Compute" {
		t.Fatalf("GetCollection = %#v, %v", collection, err)
	}
}

func TestOfferingNavigationCoversEveryEntryPoint(t *testing.T) {
	client := catalogClient(t, 1)

	items, err := collect(client.ListOfferings(t.Context(), agent.ListOptions{}))
	if err != nil || len(items) != 3 {
		t.Fatalf("ListOfferings = %d items, %v", len(items), err)
	}
	pages, err := collect(client.ListOfferingPages(t.Context(), agent.ListOptions{}))
	if err != nil || len(pages) != 3 {
		t.Fatalf("ListOfferingPages = %d pages, %v", len(pages), err)
	}
	members, err := collect(client.ListCollectionOfferings(t.Context(), "compute", agent.ListOptions{}))
	if err != nil || len(members) != 2 {
		t.Fatalf("ListCollectionOfferings = %d items, %v", len(members), err)
	}
	memberPages, err := collect(client.ListCollectionOfferingPages(t.Context(), "compute", agent.ListOptions{}))
	if err != nil || len(memberPages) != 2 {
		t.Fatalf("ListCollectionOfferingPages = %d pages, %v", len(memberPages), err)
	}
	resumed, err := collect(client.ContinueListOfferings(t.Context(), pages[0].Next, agent.ContinuationOptions{}))
	if err != nil || len(resumed) != 2 {
		t.Fatalf("ContinueListOfferings = %d items, %v", len(resumed), err)
	}
	resumedPages, err := collect(client.ContinueListOfferingPages(t.Context(), pages[0].Next, agent.ContinuationOptions{}))
	if err != nil || len(resumedPages) != 2 {
		t.Fatalf("ContinueListOfferingPages = %d pages, %v", len(resumedPages), err)
	}
	offering, err := client.GetOffering(t.Context(), "gpu", odp.RepresentationFull)
	if err != nil || offering.Name != "GPU" {
		t.Fatalf("GetOffering = %#v, %v", offering, err)
	}
}

func TestItemAndPageBudgetsStopTraversal(t *testing.T) {
	client := catalogClient(t, 1)
	items, err := collect(client.ListOfferings(t.Context(), agent.ListOptions{MaxItems: 2}))
	if err != nil || len(items) != 2 {
		t.Fatalf("MaxItems = %d items, %v", len(items), err)
	}
	pages, err := collect(client.ListOfferingPages(t.Context(), agent.ListOptions{MaxPages: 2}))
	if err != nil || len(pages) != 2 {
		t.Fatalf("MaxPages = %d pages, %v", len(pages), err)
	}
	collections, err := collect(client.ListCollections(t.Context(), agent.ListOptions{MaxItems: 1}))
	if err != nil || len(collections) != 1 {
		t.Fatalf("Collection MaxItems = %d items, %v", len(collections), err)
	}
	collectionPages, err := collect(client.ListCollectionPages(t.Context(), agent.ListOptions{MaxPages: 1}))
	if err != nil || len(collectionPages) != 1 {
		t.Fatalf("Collection MaxPages = %d pages, %v", len(collectionPages), err)
	}
}

func TestItemBudgetOnAPageBoundaryDoesNotFetchAnotherPage(t *testing.T) {
	tests := map[string]func(*agent.ServiceClient) (int, error){
		"list Collections": func(client *agent.ServiceClient) (int, error) {
			values, err := collect(client.ListCollections(t.Context(), agent.ListOptions{MaxItems: 1}))
			return len(values), err
		},
		"continue Collections": func(client *agent.ServiceClient) (int, error) {
			values, err := collect(client.ContinueListCollections(t.Context(), "/odp/collections?cursor=start", agent.ContinuationOptions{MaxItems: 1}))
			return len(values), err
		},
		"list Offerings": func(client *agent.ServiceClient) (int, error) {
			values, err := collect(client.ListOfferings(t.Context(), agent.ListOptions{MaxItems: 1}))
			return len(values), err
		},
		"continue Offerings": func(client *agent.ServiceClient) (int, error) {
			values, err := collect(client.ContinueListOfferings(t.Context(), "/odp/offerings?cursor=start", agent.ContinuationOptions{MaxItems: 1}))
			return len(values), err
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			catalogRequests := 0
			client := stubService(t, func(request *http.Request) string {
				catalogRequests++
				if strings.Contains(request.URL.Path, "collections") {
					return `{"odp_version":"1.0","items":[{"id":"compute","name":"Compute"}],"next":"/odp/collections?cursor=next"}`
				}
				return `{"odp_version":"1.0","items":[{"id":"gpu","name":"GPU"}],"next":"/odp/offerings?cursor=next"}`
			})
			count, err := run(client)
			if err != nil || count != 1 || catalogRequests != 1 {
				t.Fatalf("items = %d, requests = %d, err = %v", count, catalogRequests, err)
			}
		})
	}
}

func TestGetRequiresTheRequestedResourceIdentifier(t *testing.T) {
	client := stubService(t, func(request *http.Request) string {
		if strings.Contains(request.URL.Path, "collections") {
			return `{"odp_version":"1.0","id":"other-collection","name":"Other"}`
		}
		return `{"odp_version":"1.0","id":"other-offering","name":"Other"}`
	})
	if _, err := client.GetCollection(t.Context(), "compute", odp.RepresentationFull); err == nil || !strings.Contains(err.Error(), "identifier does not match") {
		t.Fatalf("GetCollection error = %v", err)
	}
	if _, err := client.GetOffering(t.Context(), "gpu", odp.RepresentationFull); err == nil || !strings.Contains(err.Error(), "identifier does not match") {
		t.Fatalf("GetOffering error = %v", err)
	}
}

func TestListOptionsAreValidatedBeforeAnyRequest(t *testing.T) {
	client := catalogClient(t, 1)
	cases := map[string]agent.ListOptions{
		"negative limit":    {Limit: -1},
		"limit above 100":   {Limit: 101},
		"negative items":    {MaxItems: -1},
		"items above bound": {MaxItems: 10_001},
		"negative pages":    {MaxPages: -1},
		"pages above bound": {MaxPages: 10_001},
		"unknown shape":     {Representation: "summary"},
	}
	for name, options := range cases {
		if _, err := collect(client.ListOfferings(t.Context(), options)); err == nil {
			t.Errorf("%s: offering options accepted", name)
		}
		if _, err := collect(client.ListCollections(t.Context(), options)); err == nil {
			t.Errorf("%s: collection options accepted", name)
		}
	}
	continuation := agent.ContinuationOptions{Representation: "summary"}
	if _, err := collect(client.ContinueListOfferings(t.Context(), "/odp/offerings", continuation)); err == nil {
		t.Error("offering continuation options accepted")
	}
	if _, err := collect(client.ContinueListCollections(t.Context(), "/odp/collections", continuation)); err == nil {
		t.Error("collection continuation options accepted")
	}
}

func TestUnadvertisedOperationsAreRefused(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, documentJSON("get-offering", "list-offerings"), nil, service.MediaType), nil
	})
	client, err := agent.NewServiceClient(agent.ServiceClientOptions{
		CachePartition: "test", HTTPClient: &http.Client{Transport: transport}, ServiceURL: "https://service.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(client.ListCollections(t.Context(), agent.ListOptions{})); !errors.Is(err, agent.ErrUnsupportedOperation) {
		t.Fatalf("list-collections error = %v", err)
	}
	if _, err := collect(client.SearchOfferings(t.Context(), agent.OfferingSearchOptions{Query: "x"})); !errors.Is(err, agent.ErrUnsupportedOperation) {
		t.Fatalf("search-offerings error = %v", err)
	}
	if _, err := client.GetCollection(t.Context(), "compute", odp.RepresentationFull); !errors.Is(err, agent.ErrUnsupportedOperation) {
		t.Fatalf("get-collection error = %v", err)
	}
}

// stubService answers the well-known document from one handler and every catalog path from another.
func stubService(t *testing.T, catalog func(*http.Request) string) *agent.ServiceClient {
	t.Helper()
	operations := []string{"get-collection", "get-offering", "list-collection-offerings", "list-collections", "list-offerings", "search-collections", "search-offerings"}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/.well-known/odp" {
			return response(http.StatusOK, documentJSON(operations...), nil, service.MediaType), nil
		}
		return response(http.StatusOK, catalog(request), nil, service.MediaType), nil
	})
	client, err := agent.NewServiceClient(agent.ServiceClientOptions{
		CachePartition: "test", HTTPClient: &http.Client{Transport: transport}, ServiceURL: "https://service.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestSearchSurfaceCoversBothRepresentations(t *testing.T) {
	client := stubService(t, func(request *http.Request) string {
		if strings.HasPrefix(request.URL.Path, "/odp/collections") {
			return `{"odp_version":"1.0","items":[{"id":"compute","name":"Compute"}]}`
		}
		return `{"odp_version":"1.0","items":[{"id":"gpu","name":"GPU"}]}`
	})
	offerings, err := collect(client.SearchOfferings(t.Context(), agent.OfferingSearchOptions{Query: "gpu"}))
	if err != nil || len(offerings) != 1 {
		t.Fatalf("SearchOfferings = %d, %v", len(offerings), err)
	}
	offeringPages, err := collect(client.SearchOfferingPages(t.Context(), agent.OfferingSearchOptions{Query: "gpu", Limit: 10}))
	if err != nil || len(offeringPages) != 1 {
		t.Fatalf("SearchOfferingPages = %d, %v", len(offeringPages), err)
	}
	collections, err := collect(client.SearchCollections(t.Context(), agent.CollectionSearchOptions{Query: "compute"}))
	if err != nil || len(collections) != 1 {
		t.Fatalf("SearchCollections = %d, %v", len(collections), err)
	}
	collectionPages, err := collect(client.SearchCollectionPages(t.Context(), agent.CollectionSearchOptions{Query: "compute"}))
	if err != nil || len(collectionPages) != 1 {
		t.Fatalf("SearchCollectionPages = %d, %v", len(collectionPages), err)
	}
	resumed, err := collect(client.ContinueSearchOfferings(t.Context(), "/odp/offerings/search?cursor=c", agent.ContinuationOptions{}))
	if err != nil || len(resumed) != 1 {
		t.Fatalf("ContinueSearchOfferings = %d, %v", len(resumed), err)
	}
	resumedPages, err := collect(client.ContinueSearchOfferingPages(t.Context(), "/odp/offerings/search?cursor=c", agent.ContinuationOptions{}))
	if err != nil || len(resumedPages) != 1 {
		t.Fatalf("ContinueSearchOfferingPages = %d, %v", len(resumedPages), err)
	}
	resumedCollections, err := collect(client.ContinueSearchCollections(t.Context(), "/odp/collections/search?cursor=c", agent.ContinuationOptions{}))
	if err != nil || len(resumedCollections) != 1 {
		t.Fatalf("ContinueSearchCollections = %d, %v", len(resumedCollections), err)
	}
	resumedCollectionPages, err := collect(client.ContinueSearchCollectionPages(t.Context(), "/odp/collections/search?cursor=c", agent.ContinuationOptions{}))
	if err != nil || len(resumedCollectionPages) != 1 {
		t.Fatalf("ContinueSearchCollectionPages = %d, %v", len(resumedCollectionPages), err)
	}
}

func TestRefinementsAreAcceptedOnlyWhereTheProtocolAllowsThem(t *testing.T) {
	group := `{"filter_id":"color","values":[{"value":"red","count":3}]}`
	page := func(refinements, next string) string {
		body := `{"odp_version":"1.0","items":[{"id":"gpu","name":"GPU"}]`
		if refinements != "" {
			body += `,"refinements":[` + refinements + `]`
		}
		if next != "" {
			body += `,"next":"` + next + `"`
		}
		return body + "}"
	}

	// OFR-14: the initial response of a search that asked for them.
	accepted := stubService(t, func(*http.Request) string { return page(group, "") })
	if _, err := collect(accepted.SearchOfferings(t.Context(), agent.OfferingSearchOptions{Query: "x", Refinements: []string{"color"}})); err != nil {
		t.Fatalf("requested refinements rejected: %v", err)
	}

	unrequested := stubService(t, func(*http.Request) string { return page(group, "") })
	if _, err := collect(unrequested.SearchOfferings(t.Context(), agent.OfferingSearchOptions{Query: "x"})); err == nil ||
		!strings.Contains(err.Error(), "not requested") {
		t.Fatalf("unrequested refinements error = %v", err)
	}

	// FLT-30: a group for a filter the request did not name.
	other := stubService(t, func(*http.Request) string { return page(group, "") })
	if _, err := collect(other.SearchOfferings(t.Context(), agent.OfferingSearchOptions{Query: "x", Refinements: []string{"size"}})); err == nil ||
		!strings.Contains(err.Error(), "color that was not requested") {
		t.Fatalf("foreign refinement error = %v", err)
	}

	// FLT-30: one group per filter.
	repeated := stubService(t, func(*http.Request) string { return page(group+","+group, "") })
	if _, err := collect(repeated.SearchOfferings(t.Context(), agent.OfferingSearchOptions{Query: "x", Refinements: []string{"color"}})); err == nil ||
		!strings.Contains(err.Error(), "more than once") {
		t.Fatalf("repeated refinement error = %v", err)
	}

	// OFR-15: never on a continuation, whether reached by traversal or resumed directly.
	traversed := stubService(t, func(request *http.Request) string {
		if request.URL.Query().Get("cursor") == "" {
			return page(group, "/odp/offerings/search?cursor=c")
		}
		return page(group, "")
	})
	if _, err := collect(traversed.SearchOfferings(t.Context(), agent.OfferingSearchOptions{Query: "x", Refinements: []string{"color"}})); err == nil ||
		!strings.Contains(err.Error(), "not requested") {
		t.Fatalf("continuation refinement error = %v", err)
	}
	resumed := stubService(t, func(*http.Request) string { return page(group, "") })
	if _, err := collect(resumed.ContinueSearchOfferings(t.Context(), "/odp/offerings/search?cursor=c", agent.ContinuationOptions{})); err == nil {
		t.Fatal("resumed continuation accepted refinements")
	}
}

func TestPageItemsCannotRestateTheirInheritedVersion(t *testing.T) {
	// VER-03: the version belongs to the containing document, and an item that repeats it is not
	// a nested item this Agent can treat as inheriting.
	offerings := stubService(t, func(*http.Request) string {
		return `{"odp_version":"1.0","items":[{"odp_version":"1.0","id":"gpu","name":"GPU"}]}`
	})
	if _, err := collect(offerings.ListOfferings(t.Context(), agent.ListOptions{})); err == nil ||
		!strings.Contains(err.Error(), "restate odp_version") {
		t.Fatalf("offering item error = %v", err)
	}
	collections := stubService(t, func(*http.Request) string {
		return `{"odp_version":"1.0","items":[{"odp_version":"1.0","id":"compute","name":"Compute"}]}`
	})
	if _, err := collect(collections.ListCollections(t.Context(), agent.ListOptions{})); err == nil ||
		!strings.Contains(err.Error(), "restate odp_version") {
		t.Fatalf("collection item error = %v", err)
	}
}

func TestContinuationLoopsAreRefused(t *testing.T) {
	offerings := stubService(t, func(*http.Request) string {
		return `{"odp_version":"1.0","items":[{"id":"gpu","name":"GPU"}],"next":"/odp/offerings?cursor=same"}`
	})
	if _, err := collect(offerings.ListOfferingPages(t.Context(), agent.ListOptions{})); !errors.Is(err, odp.ErrPaginationLoop) {
		t.Fatalf("offering loop error = %v", err)
	}
	collections := stubService(t, func(*http.Request) string {
		return `{"odp_version":"1.0","items":[{"id":"compute","name":"Compute"}],"next":"/odp/collections?cursor=same"}`
	})
	if _, err := collect(collections.ListCollectionPages(t.Context(), agent.ListOptions{})); !errors.Is(err, odp.ErrPaginationLoop) {
		t.Fatalf("collection loop error = %v", err)
	}
}

func TestRevalidationRequiresTheValidatorTheCacheHolds(t *testing.T) {
	stored := documentJSON("get-offering", "list-offerings")
	newClient := func(t *testing.T, followUp func(http.Header) *http.Response) *agent.ServiceClient {
		t.Helper()
		var calls atomic.Int64
		transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				headers := http.Header{"Cache-Control": []string{"max-age=0"}, "Etag": []string{`"v1"`}}
				return response(http.StatusOK, stored, headers, service.MediaType), nil
			}
			return followUp(http.Header{}), nil
		})
		client, err := agent.NewServiceClient(agent.ServiceClientOptions{
			CachePartition: "test", HTTPClient: &http.Client{Transport: transport}, ServiceURL: "https://service.example",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Inspect(t.Context()); err != nil {
			t.Fatal(err)
		}
		return client
	}

	bare := newClient(t, func(headers http.Header) *http.Response {
		return response(http.StatusNotModified, "", headers, "")
	})
	revalidated, err := bare.Inspect(t.Context())
	if err != nil || revalidated.Freshness != agent.FreshnessRevalidated {
		t.Fatalf("bare 304 = %q, %v", revalidated.Freshness, err)
	}

	// A 304 naming a different validator is confirming a representation the cache does not hold.
	mismatched := newClient(t, func(headers http.Header) *http.Response {
		headers.Set("ETag", `"v2"`)
		return response(http.StatusNotModified, "", headers, "")
	})
	if _, err := mismatched.Inspect(t.Context()); err == nil || !strings.Contains(err.Error(), "does not hold") {
		t.Fatalf("mismatched 304 error = %v", err)
	}

	// A revalidation cannot introduce a Vary the cache key does not cover.
	varying := newClient(t, func(headers http.Header) *http.Response {
		headers.Set("Vary", "X-Tenant-Id")
		headers.Set("Cache-Control", "max-age=600")
		return response(http.StatusNotModified, "", headers, "")
	})
	if _, err := varying.Inspect(t.Context()); err != nil {
		t.Fatalf("unsupported Vary on 304 = %v", err)
	}
	// The entry was dropped rather than refreshed for ten minutes, so nothing is held to
	// revalidate against on the next read.
	if _, err := varying.Inspect(t.Context()); err == nil || !strings.Contains(err.Error(), "without a cached representation") {
		t.Fatalf("inspection after dropped entry = %v", err)
	}
}

func TestUnsolicited304IsRefused(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusNotModified, "", nil, ""), nil
	})
	client, err := agent.NewServiceClient(agent.ServiceClientOptions{
		CachePartition: "test", HTTPClient: &http.Client{Transport: transport}, ServiceURL: "https://service.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Inspect(t.Context()); err == nil || !strings.Contains(err.Error(), "without a cached representation") {
		t.Fatalf("unsolicited 304 error = %v", err)
	}
}

func TestCustomHTTPClientDoesNotCacheWithoutAnExplicitPartition(t *testing.T) {
	var fetches atomic.Int64
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		fetches.Add(1)
		headers := http.Header{"Cache-Control": []string{"max-age=600"}}
		return response(http.StatusOK, documentJSON("get-offering", "list-offerings"), headers, service.MediaType), nil
	})
	shared := agent.NewMemoryCache()
	client, err := agent.NewServiceClient(agent.ServiceClientOptions{
		Cache: shared, HTTPClient: &http.Client{Transport: transport}, ServiceURL: "https://service.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := client.Inspect(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if fetches.Load() != 2 {
		t.Fatalf("fetches = %d, want 2", fetches.Load())
	}
}

func TestOversizedResponsesReportALimitFailure(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		padded := strings.TrimSuffix(documentJSON("get-offering", "list-offerings"), "}") +
			`,"extension":"` + strings.Repeat("x", 70_000) + `"}`
		return response(http.StatusOK, padded, nil, service.MediaType), nil
	})
	client, err := agent.NewServiceClient(agent.ServiceClientOptions{
		CachePartition: "test", HTTPClient: &http.Client{Transport: transport}, ServiceURL: "https://service.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A limit failure is the Agent's own, not something the Service reported.
	if _, err := client.Inspect(t.Context()); !errors.Is(err, agent.ErrResponseLimitExceeded) {
		t.Fatalf("oversized document error = %v", err)
	}
}

func TestRequestErrorDescribesItsFailure(t *testing.T) {
	cases := []struct {
		body   string
		media  string
		status int
		want   string
	}{
		{body: `{"code":"NOT_FOUND","detail":"No such Offering","status":404,"title":"Not Found","type":"https://offeringprotocol.org/problems/not-found"}`, media: "application/problem+json", status: 404, want: "No such Offering"},
		{body: `{"code":"NOT_FOUND","status":404,"title":"Not Found","type":"https://offeringprotocol.org/problems/not-found"}`, media: "application/problem+json", status: 404, want: "Not Found"},
		{body: "", media: "", status: http.StatusServiceUnavailable, want: "Service Unavailable"},
		{body: "", media: "", status: 599, want: "ODP request failed with HTTP 599"},
	}
	for _, test := range cases {
		transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
			return response(test.status, test.body, nil, test.media), nil
		})
		client, err := agent.NewServiceClient(agent.ServiceClientOptions{
			CachePartition: "test", HTTPClient: &http.Client{Transport: transport}, ServiceURL: "https://service.example",
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Inspect(t.Context())
		var failure *agent.RequestError
		if !errors.As(err, &failure) {
			t.Fatalf("status %d error = %v", test.status, err)
		}
		if failure.Error() != test.want {
			t.Errorf("status %d message = %q, want %q", test.status, failure.Error(), test.want)
		}
		if failure.Retryable != (test.status == http.StatusTooManyRequests || test.status >= 500) {
			t.Errorf("status %d retryable = %v", test.status, failure.Retryable)
		}
	}
}

func TestServiceClientOptionsAreValidated(t *testing.T) {
	cases := map[string]agent.ServiceClientOptions{
		"missing service URL": {},
		"insecure service":    {ServiceURL: "http://service.example"},
		"page size too small": {ServiceURL: "https://service.example", InitialPageSize: -1},
		"page size too large": {ServiceURL: "https://service.example", InitialPageSize: 101},
		"redirects negative":  {ServiceURL: "https://service.example", MaxRedirects: -1},
		"redirects too many":  {ServiceURL: "https://service.example", MaxRedirects: 6},
		"negative fallback":   {ServiceURL: "https://service.example", CacheFallbacks: agent.CacheFallbacks{Collection: -time.Second}},
		"negative search":     {ServiceURL: "https://service.example", CacheFallbacks: agent.CacheFallbacks{Search: -time.Second}},
		"negative schema":     {ServiceURL: "https://service.example", CacheFallbacks: agent.CacheFallbacks{AttributeSchema: -time.Second}},
		"negative capability": {ServiceURL: "https://service.example", CacheFallbacks: agent.CacheFallbacks{CapabilityDefinition: -time.Second}},
	}
	for name, options := range cases {
		if _, err := agent.NewServiceClient(options); err == nil {
			t.Errorf("%s: options accepted", name)
		}
	}
	if _, err := agent.NewServiceClient(agent.ServiceClientOptions{ServiceURL: "https://service.example"}); err != nil {
		t.Fatalf("default options rejected: %v", err)
	}
}

func TestSearchRequestsAreValidatedBeforeTheyAreSent(t *testing.T) {
	client := stubService(t, func(*http.Request) string { return `{"odp_version":"1.0","items":[]}` })
	// A request this Agent would not accept as a response is not one it sends.
	if _, err := collect(client.SearchOfferings(t.Context(), agent.OfferingSearchOptions{Sort: "not a capability identifier"})); err == nil {
		t.Fatal("malformed Offering search request sent")
	}
	if _, err := collect(client.SearchCollections(t.Context(), agent.CollectionSearchOptions{Query: strings.Repeat("q", 300)})); err == nil {
		t.Fatal("malformed Collection search request sent")
	}
}

func TestCatalogPagesAreBoundedToOneHundredItems(t *testing.T) {
	items := make([]string, 101)
	for index := range items {
		items[index] = fmt.Sprintf(`{"id":"item-%d","name":"Item"}`, index)
	}
	body := `{"odp_version":"1.0","items":[` + strings.Join(items, ",") + `]}`
	client := stubService(t, func(*http.Request) string { return body })
	// The page envelope schema carries the bound, so an over-long page is refused before the
	// Agent's own count is reached.
	if _, err := collect(client.ListOfferings(t.Context(), agent.ListOptions{})); err == nil {
		t.Fatal("oversized page accepted")
	}
}
