package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	odp "github.com/offering-protocol/odp-go"
)

// nested builds a JSON value nested `levels` deep, for exercising the depth bounds.
func nested(levels int) string {
	return strings.Repeat(`{"a":`, levels) + "1" + strings.Repeat("}", levels)
}

func TestCatalogResponsesAreHeldToTheirNestingDepthLimit(t *testing.T) {
	deep := nested(20)
	paths := map[string]string{
		"/odp/offerings":                     `{"odp_version":"1.0","items":[],"extension":` + deep + `}`,
		"/odp/collections":                   `{"odp_version":"1.0","items":[],"extension":` + deep + `}`,
		"/odp/offerings/gpu":                 `{"odp_version":"1.0","id":"gpu","name":"GPU","extension":` + deep + `}`,
		"/odp/collections/compute":           `{"odp_version":"1.0","id":"compute","name":"Compute","extension":` + deep + `}`,
		"/odp/collections/compute/offerings": `{"odp_version":"1.0","items":[],"extension":` + deep + `}`,
	}
	operations := `[{"authentication":"not-required","name":"get-collection"},{"authentication":"not-required","name":"get-offering"},{"authentication":"not-required","name":"list-collection-offerings"},{"authentication":"not-required","name":"list-collections"},{"authentication":"not-required","name":"list-offerings"}]`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", mediaTypeODP)
		if request.URL.Path == "/.well-known/odp" {
			fmt.Fprintf(writer, `{"description":"Catalog","http":{"endpoint_base":"/odp"},"language":"en","localizations":["en"],"name":"Example","odp_version":"1.0","operations":%s}`, operations)
			return
		}
		body, found := paths[request.URL.Path]
		if !found {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(writer, body)
	}))
	defer server.Close()
	client, err := NewServiceClient(ServiceClientOptions{AllowLocalNetwork: true, ServiceURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}

	// ERR-18: the bound applies to every ODP document, not only the ones with a schema that
	// happens to constrain nesting.
	drain := func(name string, sequence func(func(odp.Offering, error) bool)) {
		t.Helper()
		var failure error
		sequence(func(_ odp.Offering, err error) bool {
			failure = err
			return err == nil
		})
		if failure == nil || !strings.Contains(failure.Error(), "nesting-depth") {
			t.Errorf("%s error = %v", name, failure)
		}
	}
	drain("list-offerings", client.ListOfferings(t.Context(), ListOptions{}))
	drain("list-collection-offerings", client.ListCollectionOfferings(t.Context(), "compute", ListOptions{}))

	var collectionFailure error
	client.ListCollections(t.Context(), ListOptions{})(func(_ odp.Collection, err error) bool {
		collectionFailure = err
		return err == nil
	})
	if collectionFailure == nil || !strings.Contains(collectionFailure.Error(), "nesting-depth") {
		t.Errorf("list-collections error = %v", collectionFailure)
	}
	if _, err := client.GetOffering(t.Context(), "gpu", odp.RepresentationFull); err == nil ||
		!strings.Contains(err.Error(), "nesting-depth") {
		t.Errorf("get-offering error = %v", err)
	}
	if _, err := client.GetCollection(t.Context(), "compute", odp.RepresentationFull); err == nil ||
		!strings.Contains(err.Error(), "nesting-depth") {
		t.Errorf("get-collection error = %v", err)
	}
}

func TestContinuationResponsesAreHeldToTheSameLimits(t *testing.T) {
	deep := nested(20)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", mediaTypeODP)
		if request.URL.Path == "/.well-known/odp" {
			fmt.Fprint(writer, transportDocument)
			return
		}
		fmt.Fprint(writer, `{"odp_version":"1.0","items":[],"extension":`+deep+`}`)
	}))
	defer server.Close()
	client, err := NewServiceClient(ServiceClientOptions{AllowLocalNetwork: true, ServiceURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	var offeringFailure error
	client.ContinueListOfferings(t.Context(), "/odp/offerings?cursor=c", ContinuationOptions{})(func(_ odp.Offering, err error) bool {
		offeringFailure = err
		return err == nil
	})
	if offeringFailure == nil || !strings.Contains(offeringFailure.Error(), "nesting-depth") {
		t.Errorf("offering continuation error = %v", offeringFailure)
	}
	var collectionFailure error
	client.ContinueListCollections(t.Context(), "/odp/collections?cursor=c", ContinuationOptions{})(func(_ odp.Collection, err error) bool {
		collectionFailure = err
		return err == nil
	})
	if collectionFailure == nil || !strings.Contains(collectionFailure.Error(), "nesting-depth") {
		t.Errorf("collection continuation error = %v", collectionFailure)
	}
}

func TestCollectionScopedCapabilitiesLoadTheirCollection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", mediaTypeODP)
		if request.URL.Path == "/.well-known/odp" {
			fmt.Fprintf(writer, capabilityDocument, `{"sorts":{"inline":[`+sortJSON("by-name", "name")+`]}}`)
			return
		}
		fmt.Fprint(writer, `{"odp_version":"1.0","id":"compute","name":"Compute","search_capabilities":{"filters":{"inline":[`+filterJSON("name")+`]}}}`)
	}))
	defer server.Close()
	client, err := NewServiceClient(ServiceClientOptions{AllowLocalNetwork: true, ServiceURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	// A Service-scoped sort resolves against a filter the Collection scope contributes, so the
	// scoped call has to compose both sources rather than read the document alone.
	catalog, err := client.GetOfferingSearchCapabilities(t.Context(), "compute")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Filters) != 1 || len(catalog.Sorts) != 1 || len(catalog.Issues) != 0 {
		t.Fatalf("catalog = %#v", catalog)
	}
	if _, err := client.GetOfferingSearchCapabilities(t.Context(), "!!"); err == nil {
		t.Fatal("malformed Collection identifier accepted")
	}
}

func TestCancelledContextsStopWorkPromptly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", mediaTypeODP)
		fmt.Fprint(writer, transportDocument)
	}))
	defer server.Close()
	client, err := NewServiceClient(ServiceClientOptions{AllowLocalNetwork: true, ServiceURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.Inspect(ctx); err == nil {
		t.Fatal("inspection ran with a cancelled context")
	}

	instance, err := New(AgentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var failure error
	for _, err := range instance.SearchOfferingsAcrossServices(ctx, FederatedSearchRequest{}) {
		failure = err
		break
	}
	if failure == nil {
		t.Fatal("federated search ran with a cancelled context")
	}
}
