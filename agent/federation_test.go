package agent_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/offering-protocol/odp-go/agent"
	"github.com/offering-protocol/odp-go/directory"
	"github.com/offering-protocol/odp-go/service"
)

// oneService is a directory holding a single Service, so a federated search has one place to go.
const oneService = `{"items":[
  {"description":"Catalog","indexed_at":"2026-08-02T00:00:00Z","language":"en","localizations":["en"],"name":"Example","operations":[{"authentication":"not-required","name":"get-offering"},{"authentication":"not-required","name":"list-collection-offerings"},{"authentication":"not-required","name":"list-collections"},{"authentication":"not-required","name":"list-offerings"}],"service_origin":"https://catalog.example"}
]}`

func oneServiceDirectory(t *testing.T) *directory.Client {
	t.Helper()
	client, err := directory.New(directory.Options{
		Environment: directory.Sandbox,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return response(http.StatusOK, oneService, nil, "application/json"), nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestAFederatedSearchNarrowedToACollectionListsThatCollection(t *testing.T) {
	// Nothing in the request needs a search operation, so the Agent lists rather than searches, and
	// a named Collection makes that a listing of its members.
	var listed string
	serviceTransport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/.well-known/odp" {
			return response(http.StatusOK, documentJSON("get-offering", "list-collection-offerings", "list-collections", "list-offerings"), nil, service.MediaType), nil
		}
		listed = request.URL.Path
		return response(http.StatusOK, `{"odp_version":"1.0","items":[{"id":"gpu","name":"GPU"}]}`, nil, service.MediaType), nil
	})
	agentClient, err := agent.New(agent.AgentOptions{
		Directory: oneServiceDirectory(t),
		ServiceClient: func(_ context.Context, found directory.Service) (*agent.ServiceClient, error) {
			return agent.NewServiceClient(agent.ServiceClientOptions{
				HTTPClient: &http.Client{Transport: serviceTransport}, ServiceURL: found.ServiceOrigin,
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := 0
	for event, err := range agentClient.SearchOfferingsAcrossServices(t.Context(), agent.FederatedSearchRequest{
		Offerings: agent.OfferingSearchOptions{CollectionID: "compute"},
	}) {
		if err != nil {
			t.Fatal(err)
		}
		events++
		_ = event
	}
	if events != 1 || listed != "/odp/collections/compute/offerings" {
		t.Fatalf("events = %d, listed = %q", events, listed)
	}
}

func TestAFederatedSearchStopsWhenItsCallerDoes(t *testing.T) {
	serviceTransport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/.well-known/odp" {
			return response(http.StatusOK, documentJSON("get-offering", "list-offerings"), nil, service.MediaType), nil
		}
		return response(http.StatusOK, `{"odp_version":"1.0","items":[{"id":"gpu","name":"GPU"}]}`, nil, service.MediaType), nil
	})
	agentClient, err := agent.New(agent.AgentOptions{
		Directory: oneServiceDirectory(t),
		ServiceClient: func(_ context.Context, found directory.Service) (*agent.ServiceClient, error) {
			return agent.NewServiceClient(agent.ServiceClientOptions{
				HTTPClient: &http.Client{Transport: serviceTransport}, ServiceURL: found.ServiceOrigin,
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// A context already cancelled reaches the collector before any Service result does, so the
	// traversal reports the cancellation rather than waiting on work that will never land.
	for _, err := range agentClient.SearchOfferingsAcrossServices(ctx, agent.FederatedSearchRequest{}) {
		if err == nil {
			continue
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		return
	}
}

func TestAFederatedSearchBuildsItsOwnServiceClients(t *testing.T) {
	// Without a factory the Agent makes a client per Service itself, which is the path a caller
	// that supplies only a directory takes.
	agentClient, err := agent.New(agent.AgentOptions{Directory: oneServiceDirectory(t)})
	if err != nil {
		t.Fatal(err)
	}
	for event, err := range agentClient.SearchOfferingsAcrossServices(t.Context(), agent.FederatedSearchRequest{MaxServices: 1}) {
		// The Service does not exist, so the event is an issue rather than an Offering; what is
		// under test is that a client was built for it at all.
		if err != nil {
			t.Fatal(err)
		}
		if event.Type != agent.DiscoveryIssue {
			t.Fatalf("event = %#v", event)
		}
		return
	}
}

func TestAnOperationTheServiceDoesNotAdvertiseIsNotAttempted(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/.well-known/odp" {
			return response(http.StatusOK, documentJSON("get-offering", "list-offerings"), nil, service.MediaType), nil
		}
		t.Errorf("an unadvertised operation was requested at %s", request.URL.Path)
		return response(http.StatusOK, `{"odp_version":"1.0","items":[]}`, nil, service.MediaType), nil
	})
	client, err := agent.NewServiceClient(agent.ServiceClientOptions{
		CachePartition: "unadvertised", HTTPClient: &http.Client{Transport: transport}, ServiceURL: "https://service.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for name, drain := range map[string]func() error{
		"a Collection listing": func() error { _, err := collect(client.ListCollections(ctx, agent.ListOptions{})); return err },
		"a Collection search": func() error {
			_, err := collect(client.SearchCollections(ctx, agent.CollectionSearchOptions{Query: "x"}))
			return err
		},
		"an Offering search": func() error {
			_, err := collect(client.SearchOfferings(ctx, agent.OfferingSearchOptions{Query: "x"}))
			return err
		},
		"Collection members": func() error {
			_, err := collect(client.ListCollectionOfferings(ctx, "compute", agent.ListOptions{}))
			return err
		},
	} {
		if err := drain(); !errors.Is(err, agent.ErrUnsupportedOperation) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestAServiceDocumentThatCannotBeReadStopsEverything(t *testing.T) {
	client, err := agent.NewServiceClient(agent.ServiceClientOptions{
		CachePartition: "broken",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return response(http.StatusServiceUnavailable,
				`{"code":"UNAVAILABLE","status":503,"title":"Unavailable","type":"about:blank"}`, nil, service.ProblemMediaType), nil
		})},
		ServiceURL: "https://service.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for name, drain := range map[string]func() error{
		"a Collection listing": func() error { _, err := collect(client.ListCollections(ctx, agent.ListOptions{})); return err },
		"an Offering listing":  func() error { _, err := collect(client.ListOfferings(ctx, agent.ListOptions{})); return err },
		"search capabilities":  func() error { _, err := client.GetOfferingSearchCapabilities(ctx, ""); return err },
		"Offering details":     func() error { _, err := client.GetOfferingDetails(ctx, "gpu"); return err },
	} {
		if err := drain(); err == nil || !strings.Contains(err.Error(), "Service Unavailable") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
