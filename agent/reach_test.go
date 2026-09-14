package agent_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	odp "github.com/offering-protocol/odp-go"
	"github.com/offering-protocol/odp-go/agent"
	"github.com/offering-protocol/odp-go/service"
)

// pageOf renders a page of n Offerings, so a test can hand a client a page the protocol forbids.
func pageOf(count int) string {
	items := make([]string, count)
	for index := range items {
		items[index] = fmt.Sprintf(`{"id":"item-%d","name":"Item"}`, index)
	}
	return `{"odp_version":"1.0","items":[` + strings.Join(items, ",") + `]}`
}

func collectionPageOf(count int) string {
	items := make([]string, count)
	for index := range items {
		items[index] = fmt.Sprintf(`{"id":"item-%d","name":"Item"}`, index)
	}
	return `{"odp_version":"1.0","items":[` + strings.Join(items, ",") + `]}`
}

// failingService answers the well-known document and then fails every catalog request, so each
// entry point can be asked what it does when the Service stops answering.
func failingService(t *testing.T) *agent.ServiceClient {
	t.Helper()
	operations := []string{"get-collection", "get-offering", "list-collection-offerings", "list-collections", "list-offerings", "search-collections", "search-offerings"}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/.well-known/odp" {
			return response(http.StatusOK, documentJSON(operations...), nil, service.MediaType), nil
		}
		return nil, errors.New("the Service stopped answering")
	})
	client, err := agent.NewServiceClient(agent.ServiceClientOptions{
		CachePartition: "failing", HTTPClient: &http.Client{Transport: transport}, ServiceURL: "https://service.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestEveryEntryPointReportsAFailedRequest(t *testing.T) {
	client := failingService(t)
	ctx := t.Context()
	for name, drain := range map[string]func() error{
		"ListCollections":     func() error { _, err := collect(client.ListCollections(ctx, agent.ListOptions{})); return err },
		"ListCollectionPages": func() error { _, err := collect(client.ListCollectionPages(ctx, agent.ListOptions{})); return err },
		"SearchCollections": func() error {
			_, err := collect(client.SearchCollections(ctx, agent.CollectionSearchOptions{Query: "gpu"}))
			return err
		},
		"SearchCollectionPages": func() error {
			_, err := collect(client.SearchCollectionPages(ctx, agent.CollectionSearchOptions{Query: "gpu"}))
			return err
		},
		"ListOfferings":     func() error { _, err := collect(client.ListOfferings(ctx, agent.ListOptions{})); return err },
		"ListOfferingPages": func() error { _, err := collect(client.ListOfferingPages(ctx, agent.ListOptions{})); return err },
		"SearchOfferings": func() error {
			_, err := collect(client.SearchOfferings(ctx, agent.OfferingSearchOptions{Query: "gpu"}))
			return err
		},
		"SearchOfferingPages": func() error {
			_, err := collect(client.SearchOfferingPages(ctx, agent.OfferingSearchOptions{Query: "gpu"}))
			return err
		},
		"ListCollectionOfferings": func() error {
			_, err := collect(client.ListCollectionOfferings(ctx, "compute", agent.ListOptions{}))
			return err
		},
		"ListCollectionOfferingPages": func() error {
			_, err := collect(client.ListCollectionOfferingPages(ctx, "compute", agent.ListOptions{}))
			return err
		},
		"GetCollection": func() error { _, err := client.GetCollection(ctx, "compute", ""); return err },
		"GetOffering":   func() error { _, err := client.GetOffering(ctx, "gpu", ""); return err },
		"ContinueListCollections": func() error {
			_, err := collect(client.ContinueListCollections(ctx, "/odp/collections?cursor=c", agent.ContinuationOptions{}))
			return err
		},
		"ContinueListCollectionPages": func() error {
			_, err := collect(client.ContinueListCollectionPages(ctx, "/odp/collections?cursor=c", agent.ContinuationOptions{}))
			return err
		},
		"ContinueSearchCollections": func() error {
			_, err := collect(client.ContinueSearchCollections(ctx, "/odp/collections/search?cursor=c", agent.ContinuationOptions{}))
			return err
		},
		"ContinueListOfferings": func() error {
			_, err := collect(client.ContinueListOfferings(ctx, "/odp/offerings?cursor=c", agent.ContinuationOptions{}))
			return err
		},
		"ContinueListOfferingPages": func() error {
			_, err := collect(client.ContinueListOfferingPages(ctx, "/odp/offerings?cursor=c", agent.ContinuationOptions{}))
			return err
		},
		"ContinueSearchOfferings": func() error {
			_, err := collect(client.ContinueSearchOfferings(ctx, "/odp/offerings/search?cursor=c", agent.ContinuationOptions{}))
			return err
		},
	} {
		if err := drain(); err == nil {
			t.Errorf("%s reported no error", name)
		}
	}
}

func TestEveryEntryPointRejectsAMalformedDocument(t *testing.T) {
	// The item is missing its required name, so the page parses and its contents do not.
	client := stubService(t, func(*http.Request) string { return `{"odp_version":"1.0","items":[{"id":"one"}]}` })
	ctx := t.Context()
	for name, drain := range map[string]func() error{
		"a Collection listing": func() error { _, err := collect(client.ListCollections(ctx, agent.ListOptions{})); return err },
		"a Collection search": func() error {
			_, err := collect(client.SearchCollections(ctx, agent.CollectionSearchOptions{Query: "x"}))
			return err
		},
		"an Offering listing": func() error { _, err := collect(client.ListOfferings(ctx, agent.ListOptions{})); return err },
		"an Offering search": func() error {
			_, err := collect(client.SearchOfferings(ctx, agent.OfferingSearchOptions{Query: "x"}))
			return err
		},
		"Collection members": func() error {
			_, err := collect(client.ListCollectionOfferings(ctx, "compute", agent.ListOptions{}))
			return err
		},
		"a Collection continuation": func() error {
			_, err := collect(client.ContinueListCollections(ctx, "/odp/collections?cursor=c", agent.ContinuationOptions{}))
			return err
		},
		"an Offering continuation": func() error {
			_, err := collect(client.ContinueListOfferings(ctx, "/odp/offerings?cursor=c", agent.ContinuationOptions{}))
			return err
		},
		"a single Collection": func() error { _, err := client.GetCollection(ctx, "one", ""); return err },
		"a single Offering":   func() error { _, err := client.GetOffering(ctx, "one", ""); return err },
	} {
		if err := drain(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestPagesLargerThanTheProtocolAllowsAreRefused(t *testing.T) {
	// The page envelope schema caps items at 100, so an over-full page is refused before the
	// Agent's own guard sees it. What matters is that neither lets it through.
	offerings := stubService(t, func(*http.Request) string { return pageOf(101) })
	if _, err := collect(offerings.ListOfferings(t.Context(), agent.ListOptions{})); err == nil {
		t.Fatal("an over-full Offering page was accepted")
	}
	if _, err := collect(offerings.ContinueListOfferings(t.Context(), "/odp/offerings?cursor=c", agent.ContinuationOptions{})); err == nil {
		t.Fatal("an over-full Offering continuation was accepted")
	}
	collections := stubService(t, func(*http.Request) string { return collectionPageOf(101) })
	if _, err := collect(collections.ListCollections(t.Context(), agent.ListOptions{})); err == nil {
		t.Fatal("an over-full Collection page was accepted")
	}
	if _, err := collect(collections.ContinueListCollections(t.Context(), "/odp/collections?cursor=c", agent.ContinuationOptions{})); err == nil {
		t.Fatal("an over-full Collection continuation was accepted")
	}
}

func TestARepresentationTheRequestDidNotAskForIsRefused(t *testing.T) {
	// A terse Offering carries no Actions, so a Service that sends them has answered a different
	// request from the one that was made.
	const withActions = `{"odp_version":"1.0","items":[{"actions":[{"authentication":"not-required","http":{"href":"/rent","method":"POST"},"id":"rent","rel":"purchase"}],"id":"gpu","name":"GPU"}]}`
	client := stubService(t, func(*http.Request) string { return withActions })
	if _, err := collect(client.ListOfferings(t.Context(), agent.ListOptions{})); err == nil ||
		!strings.Contains(err.Error(), "cannot contain Actions") {
		t.Fatalf("terse page with Actions: %v", err)
	}
	single := stubService(t, func(*http.Request) string {
		return `{"odp_version":"1.0","actions":[{"authentication":"not-required","http":{"href":"/rent","method":"POST"},"id":"rent","rel":"purchase"}],"id":"gpu","name":"GPU"}`
	})
	if _, err := single.GetOffering(t.Context(), "gpu", odp.RepresentationTerse); err == nil ||
		!strings.Contains(err.Error(), "cannot contain Actions") {
		t.Fatalf("terse Offering with Actions: %v", err)
	}
	// A full Collection lists no detail_fields, because nothing is being withheld from it.
	detailed := stubService(t, func(*http.Request) string {
		return `{"odp_version":"1.0","detail_fields":["description"],"id":"compute","name":"Compute"}`
	})
	if _, err := detailed.GetCollection(t.Context(), "compute", odp.RepresentationFull); err == nil ||
		!strings.Contains(err.Error(), "detail_fields") {
		t.Fatalf("full Collection with detail_fields: %v", err)
	}
}

func TestResponsesDeeperThanTheLimitAreRefused(t *testing.T) {
	tower := strings.Repeat("[", 18) + "0" + strings.Repeat("]", 18)
	page := `{"odp_version":"1.0","items":[{"attributes":{"tower":` + tower + `},"id":"gpu","name":"GPU"}]}`
	client := stubService(t, func(*http.Request) string { return page })
	ctx := t.Context()
	for name, drain := range map[string]func() error{
		"an Offering listing":  func() error { _, err := collect(client.ListOfferings(ctx, agent.ListOptions{})); return err },
		"a Collection listing": func() error { _, err := collect(client.ListCollections(ctx, agent.ListOptions{})); return err },
		"an Offering continuation": func() error {
			_, err := collect(client.ContinueListOfferings(ctx, "/odp/offerings?cursor=c", agent.ContinuationOptions{}))
			return err
		},
		"a Collection continuation": func() error {
			_, err := collect(client.ContinueListCollections(ctx, "/odp/collections?cursor=c", agent.ContinuationOptions{}))
			return err
		},
	} {
		if err := drain(); err == nil {
			t.Errorf("%s accepted an over-deep document", name)
		}
	}
}

func TestASearchRequestTooLargeToSendIsRefused(t *testing.T) {
	client := stubService(t, func(*http.Request) string { return `{"odp_version":"1.0","items":[]}` })
	oversized := strings.Repeat("q", 70_000)
	if _, err := collect(client.SearchOfferings(t.Context(), agent.OfferingSearchOptions{Query: oversized})); err == nil {
		t.Fatal("an oversized Offering search was sent")
	}
	if _, err := collect(client.SearchCollections(t.Context(), agent.CollectionSearchOptions{Query: oversized})); err == nil {
		t.Fatal("an oversized Collection search was sent")
	}
}

func TestCallersCanStopBeforeAPageIsDrained(t *testing.T) {
	requests := 0
	client := stubService(t, func(*http.Request) string {
		requests++
		return `{"odp_version":"1.0","items":[{"id":"one","name":"One"},{"id":"two","name":"Two"}],"next":"/odp/next?cursor=c"}`
	})
	ctx := t.Context()
	for name, take := range map[string]func() int{
		"Offerings":   func() int { return takeOne(client.ListOfferings(ctx, agent.ListOptions{})) },
		"Collections": func() int { return takeOne(client.ListCollections(ctx, agent.ListOptions{})) },
		"an Offering continuation": func() int {
			return takeOne(client.ContinueListOfferings(ctx, "/odp/o?cursor=c", agent.ContinuationOptions{}))
		},
		"a Collection continuation": func() int {
			return takeOne(client.ContinueListCollections(ctx, "/odp/c?cursor=c", agent.ContinuationOptions{}))
		},
		"Offering pages":   func() int { return takeOnePage(client.ListOfferingPages(ctx, agent.ListOptions{})) },
		"Collection pages": func() int { return takeOnePage(client.ListCollectionPages(ctx, agent.ListOptions{})) },
		"an Offering page continuation": func() int {
			return takeOnePage(client.ContinueListOfferingPages(ctx, "/odp/o?cursor=c", agent.ContinuationOptions{}))
		},
	} {
		if seen := take(); seen != 1 {
			t.Errorf("%s yielded %d values after the caller stopped", name, seen)
		}
	}
}

func takeOne[Value any](sequence func(func(Value, error) bool)) int {
	seen := 0
	sequence(func(Value, error) bool {
		seen++
		return false
	})
	return seen
}

func takeOnePage[Value any](sequence func(func(Value, error) bool)) int {
	return takeOne(sequence)
}

func TestItemBudgetsStopExactlyWhereTheySay(t *testing.T) {
	client := stubService(t, func(*http.Request) string {
		return `{"odp_version":"1.0","items":[{"id":"one","name":"One"},{"id":"two","name":"Two"},{"id":"three","name":"Three"}]}`
	})
	ctx := t.Context()
	offerings, err := collect(client.ListOfferings(ctx, agent.ListOptions{MaxItems: 2}))
	if err != nil || len(offerings) != 2 {
		t.Fatalf("Offerings = %d, err = %v", len(offerings), err)
	}
	collections, err := collect(client.ListCollections(ctx, agent.ListOptions{MaxItems: 2}))
	if err != nil || len(collections) != 2 {
		t.Fatalf("Collections = %d, err = %v", len(collections), err)
	}
	continued, err := collect(client.ContinueListCollections(ctx, "/odp/collections?cursor=c", agent.ContinuationOptions{MaxItems: 1}))
	if err != nil || len(continued) != 1 {
		t.Fatalf("continued Collections = %d, err = %v", len(continued), err)
	}
	continuedOfferings, err := collect(client.ContinueListOfferings(ctx, "/odp/offerings?cursor=c", agent.ContinuationOptions{MaxItems: 1}))
	if err != nil || len(continuedOfferings) != 1 {
		t.Fatalf("continued Offerings = %d, err = %v", len(continuedOfferings), err)
	}
}
