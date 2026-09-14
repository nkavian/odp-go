package service_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	odp "github.com/offering-protocol/odp-go"
	"github.com/offering-protocol/odp-go/service"
)

// everyRoute is one request per operation the full catalog serves.
var everyRoute = []struct {
	body   string
	method string
	target string
}{
	{method: http.MethodGet, target: "/odp/offerings"},
	{method: http.MethodGet, target: "/odp/offerings/one"},
	{method: http.MethodGet, target: "/odp/collections"},
	{method: http.MethodGet, target: "/odp/collections/one"},
	{method: http.MethodGet, target: "/odp/collections/one/offerings"},
	{body: searchBody, method: http.MethodPost, target: "/odp/offerings/search"},
	{body: searchBody, method: http.MethodPost, target: "/odp/collections/search"},
}

func TestEveryRouteReportsWhatItsCatalogFailedWith(t *testing.T) {
	failing := serving(t, &answers{err: &service.Error{Code: "SERVICE_UNAVAILABLE", Message: "down", Status: http.StatusServiceUnavailable}})
	rejecting := serving(t, &answers{})
	for _, route := range everyRoute {
		response := request(t, failing, route.method, origin+route.target, strings.NewReader(route.body), odpHeaders())
		if response.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: catalog failure = %d %s", route.method, route.target, response.Code, response.Body.String())
		}
		// A malformed query is refused before the catalog is consulted at all.
		malformed := request(t, rejecting, route.method, origin+route.target+"?representation=brief", strings.NewReader(route.body), odpHeaders())
		if malformed.Code != http.StatusBadRequest {
			t.Errorf("%s %s: malformed query = %d %s", route.method, route.target, malformed.Code, malformed.Body.String())
		}
	}
}

func TestEveryPageRouteRefusesAContinuationItCannotFollow(t *testing.T) {
	const foreign = "https://evil.example/odp"
	for name, values := range map[string]*answers{
		"a list of Offerings":   {offerings: odp.Page[odp.Offering]{Next: foreign}},
		"a list of Collections": {collections: odp.Page[odp.Collection]{Next: foreign}},
		"the members of one":    {members: odp.Page[odp.Offering]{Next: foreign}},
		"a Collection search":   {searched: odp.Page[odp.Collection]{Next: foreign}},
		"an Offering search":    {search: odp.OfferingPage[odp.Offering]{Next: foreign}},
	} {
		runtime := serving(t, values)
		for _, route := range everyRoute {
			if strings.HasSuffix(route.target, "/one") {
				continue
			}
			response := request(t, runtime, route.method, origin+route.target, strings.NewReader(route.body), odpHeaders())
			if response.Code != http.StatusOK && response.Code != http.StatusInternalServerError {
				t.Errorf("%s on %s %s = %d", name, route.method, route.target, response.Code)
			}
		}
	}
	// Each page route in turn, so no envelope is left unchecked.
	for name, test := range map[string]struct {
		body   string
		method string
		target string
		values *answers
	}{
		"Offerings":          {method: http.MethodGet, target: "/odp/offerings", values: &answers{offerings: odp.Page[odp.Offering]{Next: foreign}}},
		"Collections":        {method: http.MethodGet, target: "/odp/collections", values: &answers{collections: odp.Page[odp.Collection]{Next: foreign}}},
		"Collection members": {method: http.MethodGet, target: "/odp/collections/one/offerings", values: &answers{members: odp.Page[odp.Offering]{Next: foreign}}},
		"Collection search":  {body: searchBody, method: http.MethodPost, target: "/odp/collections/search", values: &answers{searched: odp.Page[odp.Collection]{Next: foreign}}},
		"Offering search":    {body: searchBody, method: http.MethodPost, target: "/odp/offerings/search", values: &answers{search: odp.OfferingPage[odp.Offering]{Next: foreign}}},
	} {
		runtime := serving(t, test.values)
		response := request(t, runtime, test.method, origin+test.target, strings.NewReader(test.body), odpHeaders())
		if response.Code != http.StatusInternalServerError {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}
}

func TestEveryPageRouteValidatesItsItems(t *testing.T) {
	nameless := odp.Offering{ID: "one"}
	namelessCollection := odp.Collection{ID: "one"}
	for name, test := range map[string]struct {
		body   string
		method string
		target string
		values *answers
	}{
		"Collection members": {method: http.MethodGet, target: "/odp/collections/one/offerings", values: &answers{members: odp.Page[odp.Offering]{Items: []odp.Offering{nameless}}}},
		"Collection search":  {body: searchBody, method: http.MethodPost, target: "/odp/collections/search", values: &answers{searched: odp.Page[odp.Collection]{Items: []odp.Collection{namelessCollection}}}},
		"Offering search":    {body: searchBody, method: http.MethodPost, target: "/odp/offerings/search", values: &answers{search: odp.OfferingPage[odp.Offering]{Items: []odp.Offering{nameless}}}},
	} {
		runtime := serving(t, test.values)
		response := request(t, runtime, test.method, origin+test.target, strings.NewReader(test.body), odpHeaders())
		if response.Code != http.StatusInternalServerError {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}
	// A malformed identifier in a member route is a bad request, not a missing one.
	runtime := serving(t, &answers{})
	if response := get(t, runtime, "/odp/collections/one!two/offerings", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("malformed member identifier = %d", response.Code)
	}
}

func TestResponsesAreMeasuredAfterTheyAreAssembled(t *testing.T) {
	// An additive member is valid ODP, so a resource can pass validation and still be too large to
	// serve. The size and depth budgets are the last thing checked before the bytes go out.
	bulky := anOffering("one")
	bulky.Additional = odp.AdditionalMembers{"x_blob": json.RawMessage(`"` + strings.Repeat("x", 600_000) + `"`)}
	if response := get(t, serving(t, &answers{offering: &bulky}), "/odp/offerings/one", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("oversized resource = %d", response.Code)
	}
	deep := anOffering("one")
	deep.Additional = odp.AdditionalMembers{"x_tower": json.RawMessage(strings.Repeat("[", 18) + "0" + strings.Repeat("]", 18))}
	if response := get(t, serving(t, &answers{offering: &deep}), "/odp/offerings/one", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("over-deep resource = %d", response.Code)
	}
}

func TestUndeclaredRequestBodiesAreStillBounded(t *testing.T) {
	runtime := serving(t, &answers{})
	oversized := httptest.NewRequest(http.MethodPost, origin+"/odp/offerings/search",
		strings.NewReader(strings.Repeat(" ", service.MaximumRequestBodyBytes+1)))
	oversized.Header.Set("Content-Type", service.MediaType)
	// A chunked request declares no length, so the limit has to hold while the body is read.
	oversized.ContentLength = -1
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, oversized)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("undeclared over-length body = %d", recorder.Code)
	}
}

func TestEveryAdvertisedLanguageCanBeRefused(t *testing.T) {
	values := &answers{}
	runtime, err := service.New(service.Options{
		Catalog: values.catalog(),
		Document: odp.ServiceDocument{
			Description: "Multilingual", HTTP: odp.HTTPConfiguration{EndpointBase: "/odp"},
			Language: "en", Localizations: []string{"en", "fr"}, Name: "Multilingual",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The residual matches only what no other range did, so refusing every tag leaves nothing for
	// it to select and the default representation is served.
	response := get(t, runtime, "/odp/offerings", map[string]string{"Accept-Language": "*, en;q=0, fr;q=0"})
	if response.Header().Get("Content-Language") != "en" {
		t.Fatalf("Content-Language = %q", response.Header().Get("Content-Language"))
	}
}

func TestServiceConstructionRejectsWhatItCannotServe(t *testing.T) {
	full := (&answers{}).catalog()
	document := func() odp.ServiceDocument {
		return odp.ServiceDocument{
			Description: "Example", HTTP: odp.HTTPConfiguration{EndpointBase: "/odp"},
			Language: "en", Localizations: []string{"en"}, Name: "Example",
		}
	}
	if _, err := service.New(service.Options{Catalog: service.Catalog{}, Document: document()}); err == nil ||
		!strings.Contains(err.Error(), "requires ListOfferings and GetOffering") {
		t.Fatalf("empty catalog error = %v", err)
	}
	missingOffering := full
	missingOffering.GetOffering = nil
	if _, err := service.New(service.Options{Catalog: missingOffering, Document: document()}); err == nil {
		t.Fatal("catalog without GetOffering accepted")
	}
	invalid := document()
	invalid.Language = "not a language"
	if _, err := service.New(service.Options{Catalog: full, Document: invalid}); err == nil {
		t.Fatal("invalid Service Document accepted")
	}
	oversized := document()
	oversized.Additional = odp.AdditionalMembers{"x_blob": json.RawMessage(`"` + strings.Repeat("x", 70_000) + `"`)}
	if _, err := service.New(service.Options{Catalog: full, Document: oversized}); err == nil ||
		!strings.Contains(err.Error(), "resource limits") {
		t.Fatalf("oversized Service Document error = %v", err)
	}
	deep := document()
	deep.Additional = odp.AdditionalMembers{"x_tower": json.RawMessage(strings.Repeat("[", 9) + "0" + strings.Repeat("]", 9))}
	if _, err := service.New(service.Options{Catalog: full, Document: deep}); err == nil ||
		!strings.Contains(err.Error(), "resource limits") {
		t.Fatalf("over-deep Service Document error = %v", err)
	}
}

func TestStaticCatalogServesEveryOperationItAdvertises(t *testing.T) {
	runtime := staticService(t)
	document := runtime.Document()
	if len(document.Operations) != 5 {
		t.Fatalf("operations = %v", document.Operations)
	}
	// A Service search is not part of a static catalog, so its routes stay absent.
	if response := post(t, runtime, "/odp/offerings/search", searchBody, odpHeaders()); response.Code != http.StatusNotFound {
		t.Fatalf("Offering search = %d", response.Code)
	}
	if response := get(t, runtime, "/odp/offerings/missing", nil); response.Code != http.StatusNotFound {
		t.Fatalf("missing Offering = %d", response.Code)
	}
	if response := get(t, runtime, "/odp/collections/missing", nil); response.Code != http.StatusNotFound {
		t.Fatalf("missing Collection = %d", response.Code)
	}
	if response := get(t, runtime, "/odp/collections/missing/offerings", nil); response.Code != http.StatusNotFound {
		t.Fatalf("members of a missing Collection = %d", response.Code)
	}
	terse := decodeObject(t, get(t, runtime, "/odp/collections?representation=terse", nil))
	if items := terse["items"].([]any); len(items) != 1 {
		t.Fatalf("Collections = %v", items)
	}
	full := decodeObject(t, get(t, runtime, "/odp/collections/compute?representation=full", nil))
	if full["id"] != "compute" {
		t.Fatalf("Collection = %v", full)
	}
}

func TestStaticCatalogContinuationsBindTheirContext(t *testing.T) {
	runtime := staticService(t)
	next := decodeObject(t, get(t, runtime, "/odp/offerings?limit=1", nil))["next"].(string)
	for name, mutate := range map[string]func(string) string{
		"a different page size": func(value string) string { return strings.Replace(value, "limit=1", "limit=2", 1) },
		"a different representation": func(value string) string {
			return strings.Replace(value, "representation=terse", "representation=full", 1)
		},
	} {
		response := get(t, runtime, mutate(next), nil)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}
	for name, cursor := range map[string]string{
		"a cursor in one piece":          "opaque",
		"a cursor in three":              "a.b.c",
		"a signature that is not base64": "YQ.@@@@",
	} {
		response := get(t, runtime, "/odp/offerings?cursor="+cursor+"&limit=1&representation=terse", nil)
		if response.Code != http.StatusGone {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}
	// A cursor issued for one operation does not resume another.
	moved := strings.Replace(next, "/odp/offerings", "/odp/collections", 1)
	if response := get(t, runtime, moved, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("borrowed cursor = %d %s", response.Code, response.Body.String())
	}
}

func TestStaticCatalogRejectsACatalogItCannotIndex(t *testing.T) {
	valid := odp.Offering{ID: "one", Name: "One", ODPVersion: odp.Version}
	validCollection := odp.Collection{ID: "one", Name: "One", ODPVersion: odp.Version}
	for name, options := range map[string]service.StaticCatalogOptions{
		"an Offering that does not validate":  {Offerings: []odp.Offering{{ID: "one"}}},
		"a Collection that does not validate": {Collections: []odp.Collection{{ID: "one"}}},
		"two Offerings with one identifier":   {Offerings: []odp.Offering{valid, valid}},
		"two Collections with one identifier": {Collections: []odp.Collection{validCollection, validCollection}},
		"a parent that is not in the catalog": {Collections: []odp.Collection{{ID: "child", Name: "Child", ODPVersion: odp.Version, ParentIDs: []string{"absent"}}}},
	} {
		if _, err := service.NewStaticCatalog(options); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// A hierarchy deeper than the protocol allows is refused with its own reason.
	chain := make([]odp.Collection, 40)
	for index := range chain {
		chain[index] = odp.Collection{ID: fmt.Sprintf("c%d", index), Name: "C", ODPVersion: odp.Version}
		if index > 0 {
			chain[index].ParentIDs = []string{fmt.Sprintf("c%d", index-1)}
		}
	}
	if _, err := service.NewStaticCatalog(service.StaticCatalogOptions{Collections: chain}); err == nil ||
		!strings.Contains(err.Error(), "32 edges") {
		t.Fatalf("deep hierarchy error = %v", err)
	}
	// A shared parent is visited once and remembered, not walked again for every child.
	shared := []odp.Collection{
		{ID: "root", Name: "Root", ODPVersion: odp.Version},
		{ID: "left", Name: "Left", ODPVersion: odp.Version, ParentIDs: []string{"root"}},
		{ID: "right", Name: "Right", ODPVersion: odp.Version, ParentIDs: []string{"root"}},
		{ID: "leaf", Name: "Leaf", ODPVersion: odp.Version, ParentIDs: []string{"left", "right"}},
	}
	if _, err := service.NewStaticCatalog(service.StaticCatalogOptions{Collections: shared}); err != nil {
		t.Fatalf("shared parent error = %v", err)
	}
}

func TestSingleResourcesAreValidatedBeforeTheyAreServed(t *testing.T) {
	nameless := odp.Collection{ID: "one"}
	if response := get(t, serving(t, &answers{collection: &nameless}), "/odp/collections/one", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("Collection without a name = %d", response.Code)
	}
	namelessOffering := odp.Offering{ID: "one"}
	if response := get(t, serving(t, &answers{offering: &namelessOffering}), "/odp/offerings/one", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("Offering without a name = %d", response.Code)
	}
}

func TestARequestWithoutAnAuthorityIsStillAnswered(t *testing.T) {
	runtime := serving(t, &answers{offerings: odp.Page[odp.Offering]{Next: "/odp/offerings?cursor=c"}})
	// A request that names no authority at all resolves its continuation against the same empty
	// origin, so a relative reference still advances rather than reading as a jump elsewhere.
	anonymous := &http.Request{Header: http.Header{}, Method: http.MethodGet, URL: &url.URL{Path: "/odp/offerings"}}
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, anonymous)
	if recorder.Code != http.StatusOK {
		t.Fatalf("request without an authority = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestStaticCatalogRejectsUnencodableResources(t *testing.T) {
	// An additive member holding something that is not JSON cannot be copied, let alone served.
	broken := odp.AdditionalMembers{"x_broken": json.RawMessage("not json")}
	if _, err := service.NewStaticCatalog(service.StaticCatalogOptions{
		Offerings: []odp.Offering{{Additional: broken, ID: "one", Name: "One", ODPVersion: odp.Version}},
	}); err == nil {
		t.Fatal("unencodable Offering accepted")
	}
	if _, err := service.NewStaticCatalog(service.StaticCatalogOptions{
		Collections: []odp.Collection{{Additional: broken, ID: "one", Name: "One", ODPVersion: odp.Version}},
	}); err == nil {
		t.Fatal("unencodable Collection accepted")
	}
}
