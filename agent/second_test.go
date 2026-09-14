package agent_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/offering-protocol/odp-go/agent"
)

// afterTheFirstPage serves one good page carrying a continuation, then answers every later request
// with second, so the branches that only run once a traversal is already under way are reachable.
func afterTheFirstPage(t *testing.T, second string) *agent.ServiceClient {
	t.Helper()
	served := false
	return stubService(t, func(*http.Request) string {
		if served {
			return second
		}
		served = true
		return `{"odp_version":"1.0","items":[{"id":"one","name":"One"}],"next":"/odp/next?cursor=c"}`
	})
}

func TestATraversalStopsWhenAContinuationDoesNotHoldUp(t *testing.T) {
	tower := strings.Repeat("[", 18) + "0" + strings.Repeat("]", 18)
	for name, second := range map[string]string{
		"a page that is not a page":         `{"odp_version":"1.0"}`,
		"an item that does not validate":    `{"odp_version":"1.0","items":[{"id":"two"}]}`,
		"a page nested past the limit":      `{"odp_version":"1.0","items":[],"x_tower":` + tower + `}`,
		"an item that restates the version": `{"odp_version":"1.0","items":[{"id":"two","name":"Two","odp_version":"1.0"}]}`,
	} {
		ctx := t.Context()
		for entry, drain := range map[string]func(*agent.ServiceClient) error{
			"a Collection listing": func(client *agent.ServiceClient) error {
				_, err := collect(client.ListCollections(ctx, agent.ListOptions{}))
				return err
			},
			"an Offering listing": func(client *agent.ServiceClient) error {
				_, err := collect(client.ListOfferings(ctx, agent.ListOptions{}))
				return err
			},
			"a Collection continuation": func(client *agent.ServiceClient) error {
				_, err := collect(client.ContinueListCollections(ctx, "/odp/collections?cursor=a", agent.ContinuationOptions{}))
				return err
			},
			"an Offering continuation": func(client *agent.ServiceClient) error {
				_, err := collect(client.ContinueListOfferings(ctx, "/odp/offerings?cursor=a", agent.ContinuationOptions{}))
				return err
			},
			"an Offering search": func(client *agent.ServiceClient) error {
				_, err := collect(client.SearchOfferings(ctx, agent.OfferingSearchOptions{Query: "gpu"}))
				return err
			},
			"a Collection search": func(client *agent.ServiceClient) error {
				_, err := collect(client.SearchCollections(ctx, agent.CollectionSearchOptions{Query: "gpu"}))
				return err
			},
		} {
			if err := drain(afterTheFirstPage(t, second)); err == nil {
				t.Errorf("%s followed by %s was accepted", entry, name)
			}
		}
	}
}

func TestATraversalStopsWhenTheServiceStopsAnswering(t *testing.T) {
	served := false
	operations := []string{"get-collection", "get-offering", "list-collection-offerings", "list-collections", "list-offerings", "search-collections", "search-offerings"}
	build := func() *agent.ServiceClient {
		served = false
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == "/.well-known/odp" {
				return response(http.StatusOK, documentJSON(operations...), nil, "application/odp+json"), nil
			}
			if served {
				return response(http.StatusInternalServerError, `{"code":"INTERNAL_ERROR","status":500,"title":"gone","type":"https://offeringprotocol.org/problems/internal-error"}`, nil, "application/problem+json"), nil
			}
			served = true
			return response(http.StatusOK, `{"odp_version":"1.0","items":[{"id":"one","name":"One"}],"next":"/odp/next?cursor=c"}`, nil, "application/odp+json"), nil
		})
		client, err := agent.NewServiceClient(agent.ServiceClientOptions{
			CachePartition: "second", HTTPClient: &http.Client{Transport: transport}, ServiceURL: "https://service.example",
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	ctx := t.Context()
	for name, drain := range map[string]func(*agent.ServiceClient) error{
		"a Collection listing": func(client *agent.ServiceClient) error {
			_, err := collect(client.ListCollections(ctx, agent.ListOptions{}))
			return err
		},
		"an Offering listing": func(client *agent.ServiceClient) error {
			_, err := collect(client.ListOfferings(ctx, agent.ListOptions{}))
			return err
		},
		"a Collection continuation": func(client *agent.ServiceClient) error {
			_, err := collect(client.ContinueListCollections(ctx, "/odp/collections?cursor=a", agent.ContinuationOptions{}))
			return err
		},
		"an Offering continuation": func(client *agent.ServiceClient) error {
			_, err := collect(client.ContinueListOfferings(ctx, "/odp/offerings?cursor=a", agent.ContinuationOptions{}))
			return err
		},
	} {
		if err := drain(build()); err == nil {
			t.Errorf("%s kept going after the Service stopped answering", name)
		}
	}
}

func TestActionTargetsAreResolvedBeforeTheyAreOffered(t *testing.T) {
	offering := func(action string) string {
		return `{"odp_version":"1.0","id":"gpu","name":"GPU","actions":[` + action + `]}`
	}
	for name, test := range map[string]struct {
		action string
		want   string
	}{
		"an OpenAPI document over plain HTTP": {
			action: `{"authentication":"not-required","id":"rent","openapi":{"operation_id":"rent","url":"http://localhost:8080/openapi.json"},"rel":"purchase"}`,
			want:   "HTTPS",
		},
	} {
		client := stubService(t, func(*http.Request) string { return offering(test.action) })
		details, err := client.GetOfferingDetails(t.Context(), "gpu")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(details.Actions) != 0 {
			t.Errorf("%s: an unusable Action was offered", name)
		}
		if len(details.Issues) != 1 || !strings.Contains(details.Issues[0].Message, test.want) {
			t.Errorf("%s: issues = %#v", name, details.Issues)
		}
	}
}

func TestResolvingAnActionReportsWhyTheOfferingCouldNotBeRead(t *testing.T) {
	client := failingService(t)
	if _, err := client.ResolveAction(t.Context(), "gpu", "rent"); err == nil {
		t.Fatal("an Action was resolved against an unreachable Offering")
	}
}
