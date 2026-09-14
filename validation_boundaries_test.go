package odp_test

import (
	"encoding/json"
	"errors"
	"testing"

	odp "github.com/offering-protocol/odp-go"
)

func TestAgentBrandingFallbackParsesWholeService(t *testing.T) {
	for _, member := range []string{"icon", "logo"} {
		document := map[string]any{
			"odp_version": "1.0", "name": "Example", "description": "Catalog", "language": "en", "localizations": []string{"en"},
			"http":       map[string]any{"endpoint_base": "/odp"},
			"operations": []any{map[string]any{"name": "get-offering", "authentication": "not-required"}, map[string]any{"name": "list-offerings", "authentication": "not-required"}},
			"branding":   map[string]any{"icon": map[string]any{"src": "/icon", "type": "image/png"}, "logo": map[string]any{"src": "/logo", "type": "image/png"}},
		}
		document["branding"].(map[string]any)[member].(map[string]any)["type"] = "image/future"
		data, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		got, err := odp.ParseAgentServiceDocument(data)
		if err != nil || got.Name != "Example" || got.Branding != nil {
			t.Fatalf("%s: got %#v, %v", member, got, err)
		}
		if _, err := odp.ParseServiceDocument(data); err == nil {
			t.Fatal("strict parser accepted unknown branding")
		}
	}
}

func TestResourceRepresentationValidationBoundaries(t *testing.T) {
	for _, resource := range []string{"collection", "offering"} {
		for _, tc := range []struct {
			name   string
			fields map[string]any
			valid  bool
		}{
			{"invalid language", map[string]any{"language": "not_a_language"}, false},
			{"underscore locale", map[string]any{"language": "en_US"}, false},
			{"invalid syntax", map[string]any{"language": "en-!"}, false},
			{"invalid localization", map[string]any{"localizations": []string{"en", "not_a_language"}}, false},
			{"case duplicates", map[string]any{"localizations": []string{"en", "EN"}}, false},
			{"missing representation language", map[string]any{"language": "de", "localizations": []string{"en"}}, false},
			{"private language", map[string]any{"language": "x-private", "localizations": []string{"x-private"}}, true},
			{"case match", map[string]any{"language": "en", "localizations": []string{"EN"}}, true},
			{"duplicate images", map[string]any{"images": []any{map[string]any{"src": "/icon", "alt": "One"}, map[string]any{"src": "/icon", "alt": "Two"}}}, false},
		} {
			t.Run(resource+"/"+tc.name, func(t *testing.T) {
				document := map[string]any{"odp_version": "1.0", "id": "search", "name": "Search"}
				for key, value := range tc.fields {
					document[key] = value
				}
				data, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				if resource == "collection" {
					_, err = odp.ParseCollection(data)
				} else {
					_, err = odp.ParseOffering(data)
				}
				if (err == nil) != tc.valid {
					t.Fatalf("valid %v: %v", tc.valid, err)
				}
				if err != nil {
					var validation *odp.ValidationError
					if !errors.As(err, &validation) || len(validation.Issues) == 0 || validation.Error() == "" {
						t.Fatalf("missing validation details: %v", err)
					}
				}
			})
		}
	}
}

func TestFilterTypeOperatorValidation(t *testing.T) {
	for _, kind := range []string{"string", "boolean", "integer"} {
		for _, operator := range []string{"eq", "gt", "gte", "lt", "lte"} {
			data, err := json.Marshal(map[string]any{"id": "size", "title": "Size", "description": "Requested size", "type": kind, "operators": []string{operator}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = odp.ParseFilterDefinition(data)
			wantValid := kind == "integer" || operator == "eq"
			if (err == nil) != wantValid {
				t.Fatalf("%s %s: %v", kind, operator, err)
			}
		}
	}
	if _, err := odp.ParseFilterDefinition([]byte(`{"id":"available","title":"Available","description":"Availability","type":"boolean","operators":["eq"],"unit":{"system":"service","code":"flag"}}`)); err == nil {
		t.Fatal("boolean filter accepted a unit")
	}
}

func TestServiceSearchCapabilityRequiresOperation(t *testing.T) {
	for _, search := range []bool{false, true} {
		document := odp.ServiceDocument{ODPVersion: "1.0", Name: "Example", Description: "Catalog", Language: "en", Localizations: []string{"en"}, HTTP: odp.HTTPConfiguration{EndpointBase: "/odp"}, Operations: []odp.OperationDescriptor{{Name: odp.OperationGetOffering, Authentication: "not-required"}, {Name: odp.OperationListOfferings, Authentication: "not-required"}}, SearchCapabilities: &odp.SearchCapabilities{Filters: &odp.FilterCapabilitySource{Linked: &odp.CapabilityLink{Href: "/filters"}}}}
		if search {
			document.Operations = append(document.Operations, odp.OperationDescriptor{Name: odp.OperationSearchOfferings, Authentication: "not-required"})
		}
		data, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		_, err = odp.ParseServiceDocument(data)
		if (err == nil) != search {
			t.Fatalf("search=%v: %v", search, err)
		}
	}
}

func TestParserRejectsTrailingJSON(t *testing.T) {
	for _, suffix := range []string{" {}", " {", " true"} {
		if _, err := odp.ParseCollection([]byte(`{"odp_version":"1.0","id":"search","name":"Search"}` + suffix)); err == nil {
			t.Fatalf("accepted suffix %s", suffix)
		}
	}
}
