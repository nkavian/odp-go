package odp_test

import (
	"encoding/json"
	"reflect"
	"testing"

	odp "github.com/offering-protocol/odp-go"
)

func TestAdditionalMembersRoundTrip(t *testing.T) {
	models := []any{
		&odp.ServiceDocument{}, &odp.Collection{}, &odp.Offering{}, &odp.ProblemDetails{},
		&odp.FilterDefinition{}, &odp.SortDefinition{}, &odp.PricePreview{}, &odp.SortKey{},
		&odp.SearchCapabilities{}, &odp.InvalidParameter{}, &odp.FilterExpression{},
		&odp.OfferingSearchRequest{}, &odp.Page[odp.Collection]{}, &odp.OfferingPage[odp.Offering]{},
		&odp.HTTPConfiguration{}, &odp.CapabilityLink{}, &odp.FilterUnit{},
		&odp.FilterCapabilitySource{}, &odp.SortCapabilitySource{}, &odp.CollectionSearchRequest{},
		&odp.RefinementBucket{}, &odp.RefinementGroup{},
	}
	for _, model := range models {
		t.Run(reflect.TypeOf(model).Elem().Name(), func(t *testing.T) {
			baseline, err := json.Marshal(model)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(baseline, &object); err != nil {
				t.Fatal(err)
			}
			object["future"] = json.RawMessage(`{"integer":9007199254740993,"nested":[null,true,"text"]}`)
			input, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(input, model); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(model)
			if err != nil {
				t.Fatal(err)
			}
			var actual map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &actual); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, object) {
				t.Fatalf("round trip changed members: got %s, want %s", encoded, input)
			}
			for _, invalid := range []string{`[]`, `{"`, `{"odp_version":[],"id":[],"type":[],"status":[],"direction":[],"filters":[],"name":[],"items":{},"endpoint_base":[],"href":[],"system":[],"inline":{},"count":[],"filter_id":[]}`} {
				if err := model.(json.Unmarshaler).UnmarshalJSON([]byte(invalid)); err == nil {
					t.Fatalf("accepted malformed model: %s", invalid)
				}
				after, err := json.Marshal(model)
				if err != nil || string(after) != string(encoded) {
					t.Fatalf("failed decode mutated model: %s, %v", after, err)
				}
			}
			if err := json.Unmarshal(baseline, model); err != nil {
				t.Fatal(err)
			}
			after, err := json.Marshal(model)
			if err != nil || string(after) != string(baseline) {
				t.Fatalf("replacement retained stale members: %s, %v", after, err)
			}
		})
	}
}

func TestAdditionalMembersCannotOverrideCoreFields(t *testing.T) {
	offering := odp.Offering{ID: "search", Additional: odp.AdditionalMembers{
		"id": json.RawMessage(`"substitute"`), "description": json.RawMessage(`"injected"`),
		"future": json.RawMessage(`true`),
	}}
	data, err := json.Marshal(offering)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	if string(object["id"]) != `"search"` || object["description"] != nil || string(object["future"]) != "true" {
		t.Fatalf("core fields overwritten: %s", data)
	}
	offering.Additional["future"] = json.RawMessage(`{`)
	if _, err := json.Marshal(offering); err == nil {
		t.Fatal("accepted invalid extension JSON")
	}
	if _, err := json.Marshal(odp.RefinementBucket{Value: make(chan int)}); err == nil {
		t.Fatal("accepted unencodable bucket")
	}
}
