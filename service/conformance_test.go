package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	odp "github.com/offering-protocol/odp-go"
	"github.com/offering-protocol/odp-go/service"
)

// answers is a catalog that serves fixed values, so a test states exactly what a Service is asked
// to hand back and asserts on what it does with it.
type answers struct {
	collection  *odp.Collection
	collections odp.Page[odp.Collection]
	err         error
	members     odp.Page[odp.Offering]
	offering    *odp.Offering
	offerings   odp.Page[odp.Offering]
	search      odp.OfferingPage[odp.Offering]
	searched    odp.Page[odp.Collection]
	seen        *service.CatalogRequest
}

func (values *answers) catalog() service.Catalog {
	record := func(input service.CatalogRequest) {
		if values.seen != nil {
			*values.seen = input
		}
	}
	return service.Catalog{
		GetCollection: func(_ context.Context, id string, input service.CatalogRequest) (*odp.Collection, error) {
			record(input)
			if values.collection == nil {
				return nil, values.err
			}
			found := *values.collection
			if found.ID == "" {
				found.ID = id
			}
			return &found, values.err
		},
		GetOffering: func(_ context.Context, id string, input service.CatalogRequest) (*odp.Offering, error) {
			record(input)
			if values.offering == nil {
				return nil, values.err
			}
			found := *values.offering
			if found.ID == "" {
				found.ID = id
			}
			return &found, values.err
		},
		ListCollectionOfferings: func(_ context.Context, _ string, input service.CatalogRequest) (odp.Page[odp.Offering], error) {
			record(input)
			return values.members, values.err
		},
		ListCollections: func(_ context.Context, input service.CatalogRequest) (odp.Page[odp.Collection], error) {
			record(input)
			return values.collections, values.err
		},
		ListOfferings: func(_ context.Context, input service.CatalogRequest) (odp.Page[odp.Offering], error) {
			record(input)
			return values.offerings, values.err
		},
		SearchCollections: func(_ context.Context, _ *odp.CollectionSearchRequest, input service.CatalogRequest) (odp.Page[odp.Collection], error) {
			record(input)
			return values.searched, values.err
		},
		SearchOfferings: func(_ context.Context, _ *odp.OfferingSearchRequest, input service.CatalogRequest) (odp.OfferingPage[odp.Offering], error) {
			record(input)
			return values.search, values.err
		},
	}
}

func anOffering(id string) odp.Offering { return odp.Offering{ID: id, Name: "One"} }

func aCollection(id string) odp.Collection { return odp.Collection{ID: id, Name: "One"} }

const origin = "https://service.example"

const searchBody = `{"odp_version":"1.0","query":"gpu"}`

func odpHeaders() map[string]string {
	return map[string]string{"Content-Type": service.MediaType}
}

// serving builds a Service over one set of catalog answers.
func serving(t *testing.T, values *answers) *service.Service {
	t.Helper()
	return newService(t, values.catalog())
}

func get(t *testing.T, runtime http.Handler, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, runtime, http.MethodGet, origin+target, nil, headers)
}

func post(t *testing.T, runtime http.Handler, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, runtime, http.MethodPost, origin+target, strings.NewReader(body), headers)
}

func TestErrorReportsItsMessage(t *testing.T) {
	err := &service.Error{Code: "NOT_FOUND", Message: "gone", Status: http.StatusNotFound}
	if err.Error() != "gone" {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestHeadIsAnsweredByTheRouteItsGetWouldReach(t *testing.T) {
	one := anOffering("one")
	runtime := serving(t, &answers{offering: &one, offerings: odp.Page[odp.Offering]{Items: []odp.Offering{one}}})
	for _, target := range []string{"/.well-known/odp", "/odp/offerings", "/odp/offerings/one"} {
		head := request(t, runtime, http.MethodHead, origin+target, nil, nil)
		if head.Code != http.StatusOK {
			t.Fatalf("%s: HEAD = %d", target, head.Code)
		}
		if head.Header().Get("ETag") != get(t, runtime, target, nil).Header().Get("ETag") {
			t.Fatalf("%s: HEAD and GET disagree on the validator", target)
		}
	}
	// A failure answers HEAD with the status its GET would carry. Dropping the content is the
	// transport's job, which TestHeadCarriesTheHeadersOfItsGet checks over a real connection.
	if missing := request(t, runtime, http.MethodHead, origin+"/odp/nowhere", nil, nil); missing.Code != http.StatusNotFound {
		t.Fatalf("HEAD on a missing resource = %d", missing.Code)
	}
}

func TestAllowEnumeratesEveryMethodTheRouteServes(t *testing.T) {
	runtime := serving(t, &answers{})
	for target, want := range map[string]string{
		"/.well-known/odp":             "GET, HEAD",
		"/odp/offerings":               "GET, HEAD",
		"/odp/offerings/one":           "GET, HEAD",
		"/odp/collections":             "GET, HEAD",
		"/odp/collections/one":         "GET, HEAD",
		"/odp/collections/c/offerings": "GET, HEAD",
		"/odp/offerings/search":        "GET, HEAD, POST",
		"/odp/collections/search":      "GET, HEAD, POST",
	} {
		response := request(t, runtime, http.MethodDelete, origin+target, nil, nil)
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != want {
			t.Errorf("%s: %d Allow %q, want %q", target, response.Code, response.Header().Get("Allow"), want)
		}
	}
}

func TestConditionalRetrievalIsHonoured(t *testing.T) {
	runtime := serving(t, &answers{offerings: odp.Page[odp.Offering]{Items: []odp.Offering{anOffering("one")}}})
	first := get(t, runtime, "/odp/offerings", nil)
	tag := first.Header().Get("ETag")
	if tag == "" || !strings.HasPrefix(tag, `"`) {
		t.Fatalf("ETag = %q", tag)
	}
	for name, header := range map[string]string{
		"the tag itself": tag,
		"a weak tag":     "W/" + tag,
		"a wildcard":     "*",
		"one of several": `"other", ` + tag,
	} {
		response := get(t, runtime, "/odp/offerings", map[string]string{"If-None-Match": header})
		if response.Code != http.StatusNotModified || response.Body.Len() != 0 {
			t.Errorf("%s: %d with %d bytes", name, response.Code, response.Body.Len())
		}
		if response.Header().Get("ETag") != tag {
			t.Errorf("%s: 304 ETag = %q", name, response.Header().Get("ETag"))
		}
	}
	stale := get(t, runtime, "/odp/offerings", map[string]string{"If-None-Match": `"stale"`})
	if stale.Code != http.StatusOK {
		t.Fatalf("stale validator = %d", stale.Code)
	}
	// SVC-61: the validator covers the negotiated language, so two variants of one body differ.
	translated := get(t, runtime, "/odp/offerings", map[string]string{"Accept-Language": "fr"})
	if translated.Header().Get("ETag") == tag {
		t.Fatal("language variants share one validator")
	}
}

func TestMatchedPreconditionOnAnUnsafeMethodFails(t *testing.T) {
	runtime := serving(t, &answers{search: odp.OfferingPage[odp.Offering]{Items: []odp.Offering{anOffering("one")}}})
	headers := odpHeaders()
	tag := post(t, runtime, "/odp/offerings/search", searchBody, headers).Header().Get("ETag")
	if tag == "" {
		t.Fatal("search response carried no validator")
	}
	headers["If-None-Match"] = tag
	response := post(t, runtime, "/odp/offerings/search", searchBody, headers)
	if response.Code != http.StatusPreconditionFailed {
		t.Fatalf("matched precondition on POST = %d %s", response.Code, response.Body.String())
	}
	if decodeObject(t, response)["code"] != "PRECONDITION_FAILED" {
		t.Fatalf("problem = %s", response.Body.String())
	}
}

func TestCachePolicyFollowsTheOperationsAuthentication(t *testing.T) {
	open := serving(t, &answers{offerings: odp.Page[odp.Offering]{}})
	response := get(t, open, "/odp/offerings", nil)
	if response.Header().Get("Vary") != "Accept, Accept-Language" || response.Header().Get("Cache-Control") != "" {
		t.Fatalf("open headers = %v", response.Header())
	}
	values := &answers{offerings: odp.Page[odp.Offering]{}}
	closed, err := service.New(service.Options{
		Catalog: values.catalog(),
		Document: odp.ServiceDocument{
			Description: "Authenticated", HTTP: odp.HTTPConfiguration{EndpointBase: "/odp"},
			Language: "en", Localizations: []string{"en"}, Name: "Authenticated",
			Protocols: &odp.ServiceProtocols{Enrollment: []odp.EnrollmentProtocol{{Name: odp.ProtocolAEP}}},
		},
		OperationAuthentication: map[odp.Operation]odp.AuthenticationRequirement{
			odp.OperationListOfferings: odp.AuthenticationRequired,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	private := get(t, closed, "/odp/offerings", nil)
	if private.Header().Get("Vary") != "Accept, Accept-Language, Authorization" || private.Header().Get("Cache-Control") != "private" {
		t.Fatalf("authenticated headers = %v", private.Header())
	}
	// SVC-02: the well-known document stays cacheable even when every operation authenticates.
	document := get(t, closed, "/.well-known/odp", nil)
	if document.Header().Get("Cache-Control") != "" {
		t.Fatalf("well-known headers = %v", document.Header())
	}
}

func TestContinuationsAreRefusedWhenTheyCannotBeFollowed(t *testing.T) {
	for name, next := range map[string]string{
		"another origin":      "https://evil.example/odp/offerings",
		"scheme relative":     "//evil.example/odp/offerings",
		"user information":    "https://user@service.example/odp/offerings",
		"over long":           "/odp/offerings?cursor=" + strings.Repeat("c", 2_048),
		"not ASCII":           "/odp/offerings?cursor=é",
		"a control character": "/odp/offerings?cursor=a\nb",
		"unparseable":         "https://[::1/odp/offerings",
		"this request":        "/odp/offerings",
	} {
		runtime := serving(t, &answers{offerings: odp.Page[odp.Offering]{Items: []odp.Offering{anOffering("one")}, Next: next}})
		if response := get(t, runtime, "/odp/offerings", nil); response.Code != http.StatusInternalServerError {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}
	forward := serving(t, &answers{offerings: odp.Page[odp.Offering]{
		Items: []odp.Offering{anOffering("one")}, Next: "/odp/offerings?cursor=next",
	}})
	if response := get(t, forward, "/odp/offerings", nil); response.Code != http.StatusOK {
		t.Fatalf("advancing continuation = %d %s", response.Code, response.Body.String())
	}
	absolute := serving(t, &answers{collections: odp.Page[odp.Collection]{
		Items: []odp.Collection{aCollection("one")}, Next: origin + "/odp/collections?cursor=next",
	}})
	if response := get(t, absolute, "/odp/collections", nil); response.Code != http.StatusOK {
		t.Fatalf("absolute continuation = %d %s", response.Code, response.Body.String())
	}
}

func TestRefinementsAreOnlyServedWhenTheyWereAsked(t *testing.T) {
	group := func(id string) odp.RefinementGroup {
		return odp.RefinementGroup{FilterID: id, Values: []odp.RefinementBucket{{Count: 1, Value: "x"}}}
	}
	for name, test := range map[string]struct {
		body   string
		groups []odp.RefinementGroup
		want   int
	}{
		"asked for":       {body: `{"odp_version":"1.0","query":"gpu","refinements":["color"]}`, groups: []odp.RefinementGroup{group("color")}, want: http.StatusOK},
		"never asked":     {body: searchBody, groups: []odp.RefinementGroup{group("color")}, want: http.StatusInternalServerError},
		"a different one": {body: `{"odp_version":"1.0","query":"gpu","refinements":["color"]}`, groups: []odp.RefinementGroup{group("size")}, want: http.StatusInternalServerError},
		"twice":           {body: `{"odp_version":"1.0","query":"gpu","refinements":["color"]}`, groups: []odp.RefinementGroup{group("color"), group("color")}, want: http.StatusInternalServerError},
	} {
		runtime := serving(t, &answers{search: odp.OfferingPage[odp.Offering]{Items: []odp.Offering{anOffering("one")}, Refinements: test.groups}})
		if response := post(t, runtime, "/odp/offerings/search", test.body, odpHeaders()); response.Code != test.want {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}
	// OFR-15: only the initial response of a search may carry refinements.
	runtime := serving(t, &answers{search: odp.OfferingPage[odp.Offering]{Items: []odp.Offering{anOffering("one")}, Refinements: []odp.RefinementGroup{group("color")}}})
	if response := get(t, runtime, "/odp/offerings/search?cursor=c", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("continuation refinements = %d %s", response.Code, response.Body.String())
	}
}

func TestNestedItemsDoNotRestateTheVersion(t *testing.T) {
	full := odp.Offering{Description: "One", ID: "one", Language: "en", Name: "One", ODPVersion: odp.Version}
	runtime := serving(t, &answers{
		collections: odp.Page[odp.Collection]{Items: []odp.Collection{{ID: "one", Name: "One", ODPVersion: odp.Version}}},
		offering:    &full,
		offerings:   odp.Page[odp.Offering]{Items: []odp.Offering{full}},
	})
	for _, target := range []string{"/odp/offerings?representation=full", "/odp/offerings", "/odp/collections"} {
		page := decodeObject(t, get(t, runtime, target, nil))
		if page["odp_version"] != odp.Version {
			t.Fatalf("%s: page odp_version = %v", target, page["odp_version"])
		}
		for _, item := range page["items"].([]any) {
			if _, restated := item.(map[string]any)["odp_version"]; restated {
				t.Fatalf("%s: item restates odp_version", target)
			}
		}
	}
	// A single resource is a Top-Level Document and carries the version its page items must not.
	resource := decodeObject(t, get(t, runtime, "/odp/offerings/one", nil))
	if resource["odp_version"] != odp.Version {
		t.Fatalf("resource odp_version = %v", resource["odp_version"])
	}
}

func TestAnEmptyPageStillCarriesAnItemsArray(t *testing.T) {
	runtime := serving(t, &answers{})
	for _, target := range []string{"/odp/offerings", "/odp/collections"} {
		page := decodeObject(t, get(t, runtime, target, nil))
		items, ok := page["items"].([]any)
		if !ok || len(items) != 0 {
			t.Fatalf("%s: items = %#v", target, page["items"])
		}
	}
}

func TestIdentifiersAreReadVerbatim(t *testing.T) {
	runtime := serving(t, &answers{offering: &odp.Offering{Name: "One"}, collection: &odp.Collection{Name: "One"}})
	for name, target := range map[string]string{
		"a percent-encoded hyphen":    "/odp/offerings/gpu%2Dx",
		"a doubly encoded byte":       "/odp/offerings/a%2561",
		"a dot segment":               "/odp/offerings/.",
		"a parent segment":            "/odp/collections/..",
		"a character outside the set": "/odp/offerings/one!two",
		"an over-long identifier":     "/odp/offerings/" + strings.Repeat("i", 129),
	} {
		if response := get(t, runtime, target, nil); response.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}
	if response := get(t, runtime, "/odp/offerings/gpu-x", nil); response.Code != http.StatusOK {
		t.Fatalf("plain identifier = %d", response.Code)
	}
	if response := get(t, runtime, "/odp/collections/one/offerings", nil); response.Code != http.StatusOK {
		t.Fatalf("collection members = %d", response.Code)
	}
	// A route that is neither an identifier nor an operation is simply absent.
	for _, target := range []string{"/odp/offerings/one/extra", "/odp/collections/one/extra", "/odp/", "/elsewhere/offerings", "/odp/collections//offerings", "/odp/collections/a/b/offerings"} {
		if response := get(t, runtime, target, nil); response.Code != http.StatusNotFound {
			t.Errorf("%s = %d", target, response.Code)
		}
	}
}

func TestRequestedResourceMustBeTheResourceServed(t *testing.T) {
	elsewhere := anOffering("other")
	group := aCollection("other")
	runtime := serving(t, &answers{collection: &group, offering: &elsewhere})
	if response := get(t, runtime, "/odp/offerings/one", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("mismatched Offering = %d", response.Code)
	}
	if response := get(t, runtime, "/odp/collections/one", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("mismatched Collection = %d", response.Code)
	}
	absent := serving(t, &answers{})
	if response := get(t, absent, "/odp/offerings/one", nil); response.Code != http.StatusNotFound {
		t.Fatalf("absent Offering = %d", response.Code)
	}
	if response := get(t, absent, "/odp/collections/one", nil); response.Code != http.StatusNotFound {
		t.Fatalf("absent Collection = %d", response.Code)
	}
}

func TestQueryParametersAreValidated(t *testing.T) {
	var seen service.CatalogRequest
	runtime := serving(t, &answers{seen: &seen})
	for name, target := range map[string]string{
		"a repeated representation":    "/odp/offerings?representation=terse&representation=full",
		"an unknown representation":    "/odp/offerings?representation=brief",
		"a repeated limit":             "/odp/offerings?limit=1&limit=2",
		"a limit of zero":              "/odp/offerings?limit=0",
		"a limit past the cap":         "/odp/offerings?limit=101",
		"a signed limit":               "/odp/offerings?limit=%2B5",
		"a padded limit":               "/odp/offerings?limit=0005",
		"a limit that is not a number": "/odp/offerings?limit=many",
		"a repeated cursor":            "/odp/offerings?cursor=a&cursor=b",
	} {
		if response := get(t, runtime, target, nil); response.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}
	if response := get(t, runtime, "/odp/offerings?cursor=c&limit=100&representation=full", nil); response.Code != http.StatusOK {
		t.Fatalf("accepted parameters = %d", response.Code)
	}
	if seen.Cursor != "c" || seen.Limit != 100 || seen.Representation != odp.RepresentationFull {
		t.Fatalf("catalog request = %#v", seen)
	}
}

func TestSearchLimitTravelsInTheBodyOfAPost(t *testing.T) {
	var seen service.CatalogRequest
	runtime := serving(t, &answers{seen: &seen})
	if response := post(t, runtime, "/odp/offerings/search?limit=5", searchBody, odpHeaders()); response.Code != http.StatusBadRequest {
		t.Fatalf("POST with a query limit = %d %s", response.Code, response.Body.String())
	}
	if response := post(t, runtime, "/odp/offerings/search", `{"odp_version":"1.0","query":"gpu","limit":7}`, odpHeaders()); response.Code != http.StatusOK {
		t.Fatalf("POST with a body limit = %d %s", response.Code, response.Body.String())
	}
	if seen.Limit != 7 {
		t.Fatalf("limit = %d", seen.Limit)
	}
	// A GET continuation still reads its page size from the query.
	if response := get(t, runtime, "/odp/offerings/search?cursor=c&limit=9", nil); response.Code != http.StatusOK {
		t.Fatalf("GET continuation = %d %s", response.Code, response.Body.String())
	}
	if seen.Limit != 9 {
		t.Fatalf("continuation limit = %d", seen.Limit)
	}
	if response := get(t, runtime, "/odp/offerings/search", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("cursorless GET search = %d", response.Code)
	}
	if response := get(t, runtime, "/odp/collections/search", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("cursorless GET Collection search = %d", response.Code)
	}
}

func TestCollectionSearchIsServed(t *testing.T) {
	var seen service.CatalogRequest
	runtime := serving(t, &answers{
		searched: odp.Page[odp.Collection]{Items: []odp.Collection{aCollection("one")}},
		seen:     &seen,
	})
	response := post(t, runtime, "/odp/collections/search", `{"odp_version":"1.0","query":"gpu","limit":3}`, odpHeaders())
	if response.Code != http.StatusOK {
		t.Fatalf("Collection search = %d %s", response.Code, response.Body.String())
	}
	if items := decodeObject(t, response)["items"].([]any); len(items) != 1 {
		t.Fatalf("items = %v", items)
	}
	if seen.Limit != 3 {
		t.Fatalf("limit = %d", seen.Limit)
	}
	if continued := get(t, runtime, "/odp/collections/search?cursor=c", nil); continued.Code != http.StatusOK {
		t.Fatalf("Collection search continuation = %d", continued.Code)
	}
}

func TestRequestBodiesAreCheckedBeforeTheyAreParsed(t *testing.T) {
	runtime := serving(t, &answers{})
	for name, test := range map[string]struct {
		body        string
		contentType string
		want        int
	}{
		"no media type":          {body: searchBody, contentType: "", want: http.StatusUnsupportedMediaType},
		"the wrong media type":   {body: searchBody, contentType: "application/json", want: http.StatusUnsupportedMediaType},
		"a malformed media type": {body: searchBody, contentType: "application/", want: http.StatusUnsupportedMediaType},
		"not JSON":               {body: "{", contentType: service.MediaType, want: http.StatusBadRequest},
		"the wrong document":     {body: `{"odp_version":"2.0"}`, contentType: service.MediaType, want: http.StatusBadRequest},
	} {
		headers := map[string]string{}
		if test.contentType != "" {
			headers["Content-Type"] = test.contentType
		}
		if response := post(t, runtime, "/odp/offerings/search", test.body, headers); response.Code != test.want {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
		if response := post(t, runtime, "/odp/collections/search", test.body, headers); response.Code != test.want {
			t.Errorf("%s on Collections: %d %s", name, response.Code, response.Body.String())
		}
	}
	// A declared length past the limit is refused before a byte of it is read.
	declared := httptest.NewRequest(http.MethodPost, origin+"/odp/offerings/search", strings.NewReader(searchBody))
	declared.Header.Set("Content-Type", service.MediaType)
	declared.ContentLength = service.MaximumRequestBodyBytes + 1
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, declared)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared over-length body = %d", recorder.Code)
	}
}

func TestUnreadableRequestBodiesAreRejected(t *testing.T) {
	runtime := serving(t, &answers{})
	failing := httptest.NewRequest(http.MethodPost, origin+"/odp/offerings/search", errorReader{})
	failing.Header.Set("Content-Type", service.MediaType)
	failing.ContentLength = -1
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, failing)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unreadable body = %d", recorder.Code)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

func TestProblemDetailsAreBoundedAndSafe(t *testing.T) {
	long := strings.Repeat("é", 400)
	runtime := serving(t, &answers{err: &service.Error{Code: "TOO_MANY_REQUESTS", Message: long, Status: http.StatusTooManyRequests}})
	response := get(t, runtime, "/odp/offerings", nil)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	// ERR-32: a 429 must carry Retry-After.
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("429 omitted Retry-After")
	}
	// ERR-04: the title carries at most 128 code points.
	if title := decodeObject(t, response)["title"].(string); len([]rune(title)) != 128 {
		t.Fatalf("title runes = %d", len([]rune(title)))
	}

	noisy := serving(t, &answers{err: &service.Error{Code: "INVALID_REQUEST", Message: "one\nHTTP/1.1 200 OK\ttwo", Status: http.StatusBadRequest}})
	if title := decodeObject(t, get(t, noisy, "/odp/offerings", nil))["title"]; title != "one HTTP/1.1 200 OK two" {
		t.Fatalf("title = %q", title)
	}

	blank := serving(t, &answers{err: &service.Error{Code: "NOT_FOUND", Message: "   ", Status: http.StatusNotFound}})
	if title := decodeObject(t, get(t, blank, "/odp/offerings", nil))["title"]; title != "ODP request failed with HTTP 404" {
		t.Fatalf("blank title = %q", title)
	}

	unavailable := serving(t, &answers{err: &service.Error{Code: "SERVICE_UNAVAILABLE", Header: http.Header{"Retry-After": []string{"30"}}, Message: "later", Status: http.StatusServiceUnavailable}})
	after := get(t, unavailable, "/odp/offerings", nil)
	if after.Header().Get("Retry-After") != "30" {
		t.Fatalf("Retry-After = %q", after.Header().Get("Retry-After"))
	}
}

func TestMalformedProblemsBecomeTheServicesOwn(t *testing.T) {
	for name, failure := range map[string]*service.Error{
		"a status below the range": {Code: "NOT_FOUND", Header: http.Header{"X-Chosen": []string{"leak"}}, Message: "odd", Status: http.StatusOK},
		"a status above the range": {Code: "NOT_FOUND", Message: "odd", Status: 799},
		"a lowercase code":         {Code: "not_found", Message: "odd", Status: http.StatusNotFound},
		"a code with a space":      {Code: "NOT FOUND", Message: "odd", Status: http.StatusNotFound},
		"an over-long code":        {Code: strings.Repeat("A", 65), Message: "odd", Status: http.StatusNotFound},
		"an empty code":            {Message: "odd", Status: http.StatusNotFound},
	} {
		runtime := serving(t, &answers{err: failure})
		response := get(t, runtime, "/odp/offerings", nil)
		problem := decodeObject(t, response)
		if problem["code"] != "INTERNAL_ERROR" {
			t.Errorf("%s: code = %v", name, problem["code"])
		}
		if response.Header().Get("X-Chosen") != "" {
			t.Errorf("%s: kept a header chosen for a status it did not send", name)
		}
	}
	// A code the grammar accepts keeps the caller's status and headers.
	kept := serving(t, &answers{err: &service.Error{Code: "PAYMENT_REQUIRED", Header: http.Header{"X-Chosen": []string{"kept"}}, Message: "pay", Status: http.StatusPaymentRequired}})
	response := get(t, kept, "/odp/offerings", nil)
	if response.Code != http.StatusPaymentRequired || response.Header().Get("X-Chosen") != "kept" {
		t.Fatalf("kept response = %d %v", response.Code, response.Header())
	}
	if response.Header().Get("Content-Type") != service.ProblemMediaType {
		t.Fatalf("problem media type = %q", response.Header().Get("Content-Type"))
	}
}

func TestUnexpectedFailuresReachTheOperator(t *testing.T) {
	failure := errors.New("database password leaked")
	var observed error
	var observedRequest *http.Request
	values := &answers{err: failure}
	runtime, err := service.New(service.Options{
		Catalog: values.catalog(),
		Document: odp.ServiceDocument{
			Description: "Observed", HTTP: odp.HTTPConfiguration{EndpointBase: "/odp"},
			Language: "en", Localizations: []string{"en"}, Name: "Observed",
		},
		OnError: func(err error, request *http.Request) { observed, observedRequest = err, request },
	})
	if err != nil {
		t.Fatal(err)
	}
	response := get(t, runtime, "/odp/offerings", nil)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "password") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if !errors.Is(observed, failure) || observedRequest == nil {
		t.Fatalf("observed = %v", observed)
	}

	// An *Error is the Service answering, not failing, so the observer is not called for it.
	observed = nil
	expected := &answers{err: &service.Error{Code: "NOT_FOUND", Message: "gone", Status: http.StatusNotFound}}
	quiet, err := service.New(service.Options{
		Catalog: expected.catalog(),
		Document: odp.ServiceDocument{
			Description: "Observed", HTTP: odp.HTTPConfiguration{EndpointBase: "/odp"},
			Language: "en", Localizations: []string{"en"}, Name: "Observed",
		},
		OnError: func(err error, _ *http.Request) { observed = err },
	})
	if err != nil {
		t.Fatal(err)
	}
	if response := get(t, quiet, "/odp/offerings", nil); response.Code != http.StatusNotFound {
		t.Fatalf("expected failure = %d", response.Code)
	}
	if observed != nil {
		t.Fatalf("observer saw %v", observed)
	}

	// An observer that panics must not become the failure it was called about.
	panicking, err := service.New(service.Options{
		Catalog: values.catalog(),
		Document: odp.ServiceDocument{
			Description: "Observed", HTTP: odp.HTTPConfiguration{EndpointBase: "/odp"},
			Language: "en", Localizations: []string{"en"}, Name: "Observed",
		},
		OnError: func(error, *http.Request) { panic("observer") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if response := get(t, panicking, "/odp/offerings", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("panicking observer = %d", response.Code)
	}
}

func TestAcceptDecidesWhetherTheRequestCanBeAnswered(t *testing.T) {
	runtime := serving(t, &answers{})
	for name, test := range map[string]struct {
		accept string
		want   int
	}{
		"absent":                         {accept: "", want: http.StatusOK},
		"the exact media type":           {accept: service.MediaType, want: http.StatusOK},
		"a type range":                   {accept: "application/*", want: http.StatusOK},
		"everything":                     {accept: "*/*", want: http.StatusOK},
		"a weighted match":               {accept: "text/html;q=0.9, " + service.MediaType + ";q=0.8", want: http.StatusOK},
		"parameters on the type":         {accept: service.MediaType + ";charset=utf-8", want: http.StatusOK},
		"refused exactly":                {accept: service.MediaType + ";q=0, */*", want: http.StatusNotAcceptable},
		"refused as a wildcard":          {accept: "*/*;q=0", want: http.StatusNotAcceptable},
		"an unrelated type":              {accept: "text/html", want: http.StatusNotAcceptable},
		"a quality outside the grammar":  {accept: service.MediaType + ";q=2", want: http.StatusNotAcceptable},
		"a quality that is not a number": {accept: service.MediaType + ";q=high", want: http.StatusNotAcceptable},
		"a repeated weight":              {accept: "*/*;q=0, */*;q=1", want: http.StatusOK},
	} {
		headers := map[string]string{}
		if test.accept != "" {
			headers["Accept"] = test.accept
		}
		if response := get(t, runtime, "/odp/offerings", headers); response.Code != test.want {
			t.Errorf("%s: %d", name, response.Code)
		}
	}
}

func TestLanguageIsSelectedByLookup(t *testing.T) {
	var seen service.CatalogRequest
	values := &answers{seen: &seen}
	runtime, err := service.New(service.Options{
		Catalog: values.catalog(),
		Document: odp.ServiceDocument{
			Description: "Multilingual", HTTP: odp.HTTPConfiguration{EndpointBase: "/odp"},
			Language: "en", Localizations: []string{"en", "fr", "de-CH"}, Name: "Multilingual",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		accept string
		want   string
	}{
		"an exact tag":              {accept: "fr", want: "fr"},
		"a tag to truncate":         {accept: "fr-CA", want: "fr"},
		"a singleton to drop":       {accept: "de-CH-x-private", want: "de-CH"},
		"the strongest preference":  {accept: "de-CH;q=0.2, fr;q=0.9", want: "fr"},
		"nothing this Service has":  {accept: "ja", want: "en"},
		"the residual":              {accept: "ja, *;q=0.1", want: "en"},
		"a residual past a refusal": {accept: "en;q=0, *", want: "fr"},
		"no header at all":          {accept: "", want: "en"},
		"a malformed weight":        {accept: "fr;q=nine", want: "en"},
		"an empty range":            {accept: ";q=0.5, fr", want: "fr"},
	} {
		headers := map[string]string{}
		if test.accept != "" {
			headers["Accept-Language"] = test.accept
		}
		response := get(t, runtime, "/odp/offerings", headers)
		if response.Header().Get("Content-Language") != test.want {
			t.Errorf("%s: Content-Language = %q, want %q", name, response.Header().Get("Content-Language"), test.want)
		}
	}
	// OFR-20: a resource that declares its own language is served as that language.
	declared := &odp.Offering{ID: "one", Language: "de-CH", Name: "One"}
	localized := serving(t, &answers{offering: declared})
	if response := get(t, localized, "/odp/offerings/one", map[string]string{"Accept-Language": "fr"}); response.Header().Get("Content-Language") != "de-CH" {
		t.Fatalf("declared language = %q", response.Header().Get("Content-Language"))
	}
}

func TestOptionalOperationsAreAdvertisedAndRoutedTogether(t *testing.T) {
	minimal := service.Catalog{
		GetOffering: func(context.Context, string, service.CatalogRequest) (*odp.Offering, error) {
			return nil, nil
		},
		ListOfferings: func(context.Context, service.CatalogRequest) (odp.Page[odp.Offering], error) {
			return odp.Page[odp.Offering]{}, nil
		},
	}
	runtime := newService(t, minimal)
	if len(runtime.Document().Operations) != 2 {
		t.Fatalf("operations = %v", runtime.Document().Operations)
	}
	for _, target := range []string{"/odp/collections", "/odp/collections/one", "/odp/collections/one/offerings"} {
		if response := get(t, runtime, target, nil); response.Code != http.StatusNotFound {
			t.Errorf("%s = %d", target, response.Code)
		}
	}
	for _, target := range []string{"/odp/offerings/search", "/odp/collections/search"} {
		if response := post(t, runtime, target, searchBody, odpHeaders()); response.Code != http.StatusNotFound {
			t.Errorf("%s = %d", target, response.Code)
		}
	}
}

func TestPagesAndItemsAreValidatedBeforeTheyAreServed(t *testing.T) {
	crowded := make([]odp.Offering, 101)
	for index := range crowded {
		crowded[index] = anOffering(fmt.Sprintf("item-%d", index))
	}
	crowdedCollections := make([]odp.Collection, 101)
	for index := range crowdedCollections {
		crowdedCollections[index] = aCollection(fmt.Sprintf("item-%d", index))
	}
	withActions := odp.Offering{
		Actions: []odp.Action{{Authentication: odp.AuthenticationNotRequired, HTTP: &odp.HTTPActionTarget{Href: "/rent", Method: http.MethodPost}, ID: "rent", Rel: odp.ActionPurchase}},
		ID:      "one", Name: "One",
	}
	repeated := withActions
	repeated.Actions = append(append([]odp.Action{}, withActions.Actions...), withActions.Actions[0])

	for name, test := range map[string]struct {
		target string
		values *answers
	}{
		"more Offerings than a page holds":   {target: "/odp/offerings", values: &answers{offerings: odp.Page[odp.Offering]{Items: crowded}}},
		"more Collections than a page holds": {target: "/odp/collections", values: &answers{collections: odp.Page[odp.Collection]{Items: crowdedCollections}}},
		"an Offering without a name":         {target: "/odp/offerings", values: &answers{offerings: odp.Page[odp.Offering]{Items: []odp.Offering{{ID: "one"}}}}},
		"a Collection without a name":        {target: "/odp/collections", values: &answers{collections: odp.Page[odp.Collection]{Items: []odp.Collection{{ID: "one"}}}}},
		"Actions in a terse Offering":        {target: "/odp/offerings", values: &answers{offerings: odp.Page[odp.Offering]{Items: []odp.Offering{withActions}}}},
		"detail_fields in a full Offering":   {target: "/odp/offerings?representation=full", values: &answers{offerings: odp.Page[odp.Offering]{Items: []odp.Offering{{DetailFields: []string{"attributes"}, ID: "one", Name: "One"}}}}},
		"detail_fields in a full Collection": {target: "/odp/collections?representation=full", values: &answers{collections: odp.Page[odp.Collection]{Items: []odp.Collection{{DetailFields: []string{"description"}, ID: "one", Name: "One"}}}}},
		"repeated Action identifiers":        {target: "/odp/offerings/one", values: &answers{offering: &repeated}},
		"members of an unknown page shape":   {target: "/odp/collections/one/offerings", values: &answers{members: odp.Page[odp.Offering]{Items: []odp.Offering{{ID: "one"}}}}},
	} {
		runtime := serving(t, test.values)
		if response := get(t, runtime, test.target, nil); response.Code != http.StatusInternalServerError {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}
}

func TestResponsesStayInsideTheirResourceLimits(t *testing.T) {
	bulky := make([]odp.Offering, 100)
	for index := range bulky {
		item := anOffering(fmt.Sprintf("item-%d", index))
		item.Attributes = map[string]json.RawMessage{"blob": json.RawMessage(`"` + strings.Repeat("x", 6_000) + `"`)}
		bulky[index] = item
	}
	runtime := serving(t, &answers{offerings: odp.Page[odp.Offering]{Items: bulky}})
	if response := get(t, runtime, "/odp/offerings?representation=full", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("oversized page = %d", response.Code)
	}

	nested := json.RawMessage(strings.Repeat("[", 18) + "0" + strings.Repeat("]", 18))
	deep := anOffering("one")
	deep.Attributes = map[string]json.RawMessage{"tower": nested}
	tower := serving(t, &answers{offering: &deep})
	if response := get(t, tower, "/odp/offerings/one", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("over-deep resource = %d", response.Code)
	}
}
