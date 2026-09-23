package directory_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	odp "github.com/offering-protocol/odp-go"
	"github.com/offering-protocol/odp-go/directory"
)

func importedResult(kind, sourceType string) map[string]any {
	result := mixedResult(kind)
	result["service"] = map[string]any{
		"service_id": "openapi-service", "service_origin": "https://api.example.com",
		"name": "Imported API", "indexed_at": "2026-09-22T12:00:00Z",
		"source": map[string]any{"type": sourceType, "url": "https://docs.example.com/v1/openapi.json?revision=2", "x402_discovery": true, "extra": 1},
	}
	return result
}

func TestSourceAwareSearchAndContinuation(t *testing.T) {
	for _, sourceType := range []string{"openapi", "future"} {
		for _, continuation := range []bool{false, true} {
			t.Run(sourceType+map[bool]string{false: "/search", true: "/continue"}[continuation], func(t *testing.T) {
				item := importedResult("collection", sourceType)
				service := item["service"].(map[string]any)
				service["operations"] = []any{map[string]any{"name": "get-collection", "authentication": "not-required"}}
				service["http"] = map[string]any{"endpoint_base": "/fabricated"}
				service["extra"] = "retained"
				value := client(t, func(request *http.Request) (*http.Response, error) {
					if continuation && request.Method != http.MethodGet {
						t.Fatalf("method = %s", request.Method)
					}
					return response(200, mixedBody(t, item, mixedResult("service")), nil), nil
				}, directory.Production)
				search := value.Search(t.Context(), directory.DirectorySearchRequest{}, directory.IterationOptions{})
				if continuation {
					search = value.ContinueSearch(t.Context(), "/v1/directory/search?cursor=x", directory.IterationOptions{})
				}
				for page, err := range search.Responses {
					if err != nil || len(page.Items) != 2 || len(page.Issues) != 0 {
						t.Fatalf("page=%#v err=%v", page, err)
					}
					got := page.Items[0].Service
					if got.Source.Type != directory.SourceType(sourceType) || got.Source.URL != "https://docs.example.com/v1/openapi.json?revision=2" || !got.Source.X402Discovery || string(got.Source.Additional["extra"]) != "1" {
						t.Fatalf("source=%#v", got.Source)
					}
					if got.Description != "" || got.Language != "" || got.Localizations != nil || got.Operations != nil || got.Protocols != nil || got.Additional["http"] != nil || got.Additional["source"] != nil || got.Additional["service_id"] != nil || string(got.Additional["extra"]) != `"retained"` {
						t.Fatalf("metadata=%#v", got)
					}
					if page.Items[0].Collection.ID != "Weather" || page.Items[1].Service.Source.Type != directory.SourceODP || page.Items[1].Service.Source.X402Discovery {
						t.Fatalf("results=%#v", page.Items)
					}
				}
			})
		}
	}
}

func TestImportedMetadataAndProtocols(t *testing.T) {
	item := importedResult("service", "openapi")
	service := item["service"].(map[string]any)
	for key, value := range map[string]any{
		"description": "API description", "documentation_url": "https://docs.example.com/", "keywords": []string{"weather"},
		"language": "en", "localizations": []string{"en"}, "status_url": "https://status.example.com/",
		"support_url": "https://example.com/support/", "website_url": "https://example.com/",
		"protocols": map[string]any{
			"enrollment": []any{map[string]any{"name": "future"}, map[string]any{"name": "aep"}},
			"payments":   []any{map[string]any{"name": "x402", "authentication": "not-required", "options": []string{"solana"}}},
			"trust":      []any{map[string]any{"name": "tap"}},
		},
	} {
		service[key] = value
	}
	value := client(t, func(*http.Request) (*http.Response, error) { return response(200, mixedBody(t, item), nil), nil }, directory.Production)
	for page, err := range value.Search(t.Context(), directory.DirectorySearchRequest{}, directory.IterationOptions{}).Responses {
		if err != nil || len(page.Items) != 1 || len(page.Issues) != 0 {
			t.Fatalf("page=%#v err=%v", page, err)
		}
		got := page.Items[0].Service
		if got.Description != "API description" || got.DocumentationURL != "https://docs.example.com/" || got.Language != "en" || len(got.Localizations) != 1 || len(got.Keywords) != 1 || got.StatusURL == "" || got.SupportURL == "" || got.WebsiteURL == "" || len(got.Protocols.Enrollment) != 1 || got.Protocols.Payments[0].Name != odp.ProtocolX402 || got.Protocols.Trust[0].Name != odp.ProtocolTAP {
			t.Fatalf("metadata=%#v", got)
		}
	}
}

func TestSourceRecordFailuresAreIsolated(t *testing.T) {
	mutations := map[string]func(map[string]any){
		"missing source":          func(v map[string]any) { delete(v, "source") },
		"null source":             func(v map[string]any) { v["source"] = nil },
		"invalid source":          func(v map[string]any) { v["source"] = true },
		"missing id":              func(v map[string]any) { delete(v, "service_id") },
		"missing name":            func(v map[string]any) { delete(v, "name") },
		"null name":               func(v map[string]any) { v["name"] = nil },
		"missing timestamp":       func(v map[string]any) { delete(v, "indexed_at") },
		"bad timestamp":           func(v map[string]any) { v["indexed_at"] = "yesterday" },
		"invalid origin":          func(v map[string]any) { v["service_origin"] = "http://localhost" },
		"native missing metadata": func(v map[string]any) { v["source"].(map[string]any)["type"] = "odp" },
	}
	for _, field := range []string{"description", "documentation_url", "keywords", "language", "localizations", "status_url", "support_url", "website_url"} {
		mutations[field+" wrong type"] = func(v map[string]any) { v[field] = 1 }
		mutations[field+" null"] = func(v map[string]any) { v[field] = nil }
	}
	for field, invalid := range map[string][]any{
		"type": {nil, "", 1}, "url": {nil, "", "relative.json", "http://example.com/openapi.json", "https://localhost/openapi.json", "https://127.0.0.1/openapi.json", "https://10.0.0.1/openapi.json", "https://user:password@example.com/openapi.json", "https://example.com/openapi.json#tag", "https://example.com/openapi.json#", "https://example.com/%bad%"},
		"x402_discovery": {nil, "true", 1},
	} {
		for index, bad := range invalid {
			key, _ := json.Marshal([]any{field, index})
			mutations[string(key)] = func(v map[string]any) { v["source"].(map[string]any)[field] = bad }
		}
		mutations["missing "+field] = func(v map[string]any) { delete(v["source"].(map[string]any), field) }
	}
	for _, category := range []string{"enrollment", "payments", "trust"} {
		for index, bad := range []any{nil, true, []any{nil}, []any{map[string]any{}}, []any{map[string]any{"name": 1}}, []any{map[string]any{"name": map[string]string{"enrollment": "aep", "payments": "x402", "trust": "tap"}[category], "extra": true}}} {
			key, _ := json.Marshal([]any{category, index})
			mutations[string(key)] = func(v map[string]any) { v["protocols"] = map[string]any{category: bad} }
		}
	}
	mutations["protocols null"] = func(v map[string]any) { v["protocols"] = nil }
	mutations["protocols invalid"] = func(v map[string]any) { v["protocols"] = true }
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			item := importedResult("service", "openapi")
			mutate(item["service"].(map[string]any))
			value := client(t, func(*http.Request) (*http.Response, error) {
				return response(200, mixedBody(t, item, mixedResult("service")), nil), nil
			}, directory.Production)
			for page, err := range value.Search(t.Context(), directory.DirectorySearchRequest{}, directory.IterationOptions{}).Responses {
				if err != nil || len(page.Items) != 1 || len(page.Issues) != 1 || page.Issues[0].Index != 0 {
					t.Fatalf("page=%#v err=%v", page, err)
				}
			}
		})
	}
}

func TestSourceFiltersSearchAndSuggest(t *testing.T) {
	for _, sources := range [][]directory.SourceType{nil, {directory.SourceODP}, {directory.SourceOpenAPI}, {directory.SourceODP, directory.SourceOpenAPI}} {
		value := client(t, func(request *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Filters directory.ServiceFilters `json:"filters"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(sources)
			got, _ := json.Marshal(payload.Filters.Sources)
			if string(encoded) != string(got) || len(payload.Filters.Payments) != 1 {
				t.Fatalf("body=%s", body)
			}
			if strings.HasSuffix(request.URL.Path, "/suggestions") {
				return response(200, `{"items":["Weather"]}`, nil), nil
			}
			return response(200, `{"items":[]}`, nil), nil
		}, directory.Production)
		filters := &directory.ServiceFilters{Sources: sources, Payments: []directory.PaymentFilter{{Name: odp.ProtocolX402}}}
		for _, err := range value.Search(t.Context(), directory.DirectorySearchRequest{SearchRequest: directory.SearchRequest{Filters: filters}}, directory.IterationOptions{}).Responses {
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, err := range value.SearchServices(t.Context(), directory.SearchRequest{Filters: filters}, directory.IterationOptions{}).Responses {
			if err != nil {
				t.Fatal(err)
			}
		}
		names, err := value.Suggest(t.Context(), directory.SuggestionRequest{Prefix: "we", Filters: filters})
		if err != nil || len(names) != 1 || names[0] != "Weather" {
			t.Fatalf("names=%v err=%v", names, err)
		}
	}
}

func TestInvalidSourceFiltersDoNotSendRequests(t *testing.T) {
	value := client(t, func(*http.Request) (*http.Response, error) { t.Fatal("invalid filters sent"); return nil, nil }, directory.Production)
	for _, sources := range [][]directory.SourceType{{}, {"future"}, {"ODP"}, {""}, {directory.SourceODP, directory.SourceODP}, {directory.SourceODP, directory.SourceOpenAPI, "future"}} {
		filters := &directory.ServiceFilters{Sources: sources}
		for _, err := range value.Search(t.Context(), directory.DirectorySearchRequest{SearchRequest: directory.SearchRequest{Filters: filters}}, directory.IterationOptions{}).Responses {
			if err == nil {
				t.Fatal("accepted invalid filters")
			}
		}
		for _, err := range value.SearchServices(t.Context(), directory.SearchRequest{Filters: filters}, directory.IterationOptions{}).Responses {
			if err == nil {
				t.Fatal("accepted invalid filters")
			}
		}
		if _, err := value.Suggest(t.Context(), directory.SuggestionRequest{Prefix: "we", Filters: filters}); err == nil {
			t.Fatal("accepted invalid filters")
		}
	}
}

func TestSourceFiltersCapturedBeforeIteration(t *testing.T) {
	value := client(t, func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil || string(body) != `{"filters":{"sources":["openapi"]}}` {
			t.Fatalf("body=%s err=%v", body, err)
		}
		return response(200, `{"items":[]}`, nil), nil
	}, directory.Production)
	sources := []directory.SourceType{directory.SourceOpenAPI}
	search := value.Search(t.Context(), directory.DirectorySearchRequest{SearchRequest: directory.SearchRequest{Filters: &directory.ServiceFilters{Sources: sources}}}, directory.IterationOptions{})
	sources[0] = directory.SourceODP
	for _, err := range search.Responses {
		if err != nil {
			t.Fatal(err)
		}
	}
}
