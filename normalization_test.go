package odp_test

import (
	"encoding/json"
	"reflect"
	"testing"

	odp "github.com/offering-protocol/odp-go"
)

func TestAgentNormalizationPreservesUsableCapabilities(t *testing.T) {
	for _, tc := range []struct{ name, kind, input, want string }{
		{"branding extensions", "service-document", `{"branding":{"future":true,"icon":{"src":"/i","future":1},"logo":{"src":"/l","type":"image/png","future":2}}}`, `{"branding":{"icon":{"src":"/i"},"logo":{"src":"/l","type":"image/png"}}}`},
		{"unknown branding icon", "service-document", `{"branding":{"icon":{"src":"/i","type":"future"},"logo":{"src":"/l","type":"image/png"}}}`, `{}`},
		{"unknown branding logo", "service-document", `{"branding":{"icon":{"src":"/i","type":"image/png"},"logo":{"src":"/l","type":"future"}}}`, `{}`},
		{"empty branding", "service-document", `{"branding":{"future":true}}`, `{}`},
		{"payment options", "service-document", `{"protocols":{"payments":[{"name":"mpp","authentication":"required","options":["future","inflow"]},{"name":"x402","authentication":"optional","options":["future"]}]}}`, `{"protocols":{"payments":[{"name":"mpp","authentication":"required","options":["inflow"]},{"name":"x402","authentication":"optional"}]}}`},
		{"images", "collection", `{"images":[{"src":"/a","future":true},{"src":"/b","type":"future"},null]}`, `{"images":[{"src":"/a"},null]}`},
		{"filter types", "filter-page", `{"items":[{"id":"known","type":"integer"},{"type":"future"},{"operators":["future"]},{"unit":{"system":"future"}},null]}`, `{"items":[{"id":"known","type":"integer"},null]}`},
		{"filter vocabulary", "filter-page", `{"items":[{"type":"string","operators":["eq","exists","in"],"unit":{"system":"service"}},{"type":"decimal","operators":["gt","gte","lt","lte"],"unit":{"system":"ucum"}}]}`, `{"items":[{"type":"string","operators":["eq","exists","in"],"unit":{"system":"service"}},{"type":"decimal","operators":["gt","gte","lt","lte"],"unit":{"system":"ucum"}}]}`},
		{"sort vocabulary", "sort-page", `{"items":[{"keys":[{"direction":"ascending","missing":"last"}]},{"keys":[{"direction":"descending","missing":"first"}]},{"keys":[{"direction":"future"}]},{"keys":[{"missing":"future"}]},{"keys":[null]},{}]}`, `{"items":[{"keys":[{"direction":"ascending","missing":"last"}]},{"keys":[{"direction":"descending","missing":"first"}]},{"keys":[null]},{}]}`},
		{"inline definitions", "collection", `{"search_capabilities":{"filters":{"inline":[{"type":"future"},{"type":"integer"},null]},"sorts":{"inline":[{"keys":[{"direction":"future"}]}]}}}`, `{"search_capabilities":{"filters":{"inline":[{"type":"integer"},null]}}}`},
		{"empty capabilities", "offering", `{"search_capabilities":{"filters":{"inline":[{"type":"future"}]}}}`, `{}`},
		{"linked capabilities", "service-document", `{"search_capabilities":{"filters":{"linked":{"href":"/filters"}}}}`, `{"search_capabilities":{"filters":{"linked":{"href":"/filters"}}}}`},
		{"problem locations", "problem", `{"invalid_params":[{"in":"body"},{"in":"header"},{"in":"path"},{"in":"query"},{"in":"future"},null,{}]}`, `{"invalid_params":[{"in":"body"},{"in":"header"},{"in":"path"},{"in":"query"},null,{}]}`},
		{"future price and schema", "offering", `{"price":{"type":"future"},"schema":{"url":"/schema","future":true},"id":"search"}`, `{"id":"search"}`},
		{"action boundaries", "offering", `{"actions":[{"id":"keep","rel":"future","http":{"method":"POST","href":"/run"}},{"authentication":"future"},{"future":true},{"http":{"future":true}},{"http":{"request":{"future":true}}},{"http":{"request":{"schema":{"future":true}}}},{"openapi":{"future":true}},{"http":{"method":"DELETE"}}]}`, `{"actions":[{"id":"keep","rel":"future","http":{"method":"POST","href":"/run"}}]}`},
		{"empty actions", "offering", `{"actions":[{"http":{"method":"PATCH"}}]}`, `{}`},
		{"nested offerings", "offering-page", `{"items":[{"id":"one","price":{"type":"future"}},{"id":"two","price":{"type":"free"}},null]}`, `{"items":[{"id":"one"},{"id":"two","price":{"type":"free"}},null]}`},
		{"nested collections", "collection-page", `{"items":[{"images":[{"src":"/x","type":"future"}]}]}`, `{"items":[{}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(tc.input)
			got, err := odp.NormalizeAgentResponse(input, tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			var actual, expected any
			if err := json.Unmarshal(got, &actual); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if string(input) != tc.input {
				t.Fatal("mutated caller input")
			}
		})
	}
}

func TestAgentNormalizationLeavesMalformedValuesForValidation(t *testing.T) {
	for _, kind := range []string{"service-document", "collection", "offering", "filter-page", "sort-page", "problem", "collection-page", "offering-page"} {
		t.Run(kind, func(t *testing.T) {
			for _, input := range []string{`{}`, `{"items":null}`, `{"items":42}`, `{"invalid_params":null}`} {
				got, err := odp.NormalizeAgentResponse([]byte(input), kind)
				if err != nil || string(got) != input {
					t.Fatalf("%s: got %s, %v", input, got, err)
				}
			}
		})
	}
	for _, input := range []string{`{`, `[]`, `true`} {
		if _, err := odp.NormalizeAgentResponse([]byte(input), "service-document"); err == nil {
			t.Fatalf("accepted %s", input)
		}
		if _, err := odp.ParseAgentServiceDocument([]byte(input)); err == nil {
			t.Fatalf("parsed %s", input)
		}
	}
}
