package service_test

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	odp "github.com/offering-protocol/odp-go"
	"github.com/offering-protocol/odp-go/service"
)

func TestHeadCarriesTheHeadersOfItsGet(t *testing.T) {
	one := anOffering("one")
	runtime := serving(t, &answers{offering: &one, offerings: odp.Page[odp.Offering]{Items: []odp.Offering{one}}})
	server := httptest.NewServer(runtime)
	defer server.Close()
	for _, path := range []string{"/.well-known/odp", "/odp/offerings", "/odp/offerings/one"} {
		body, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(body.Body)
		body.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		head, err := http.Head(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		head.Body.Close()
		if head.StatusCode != http.StatusOK {
			t.Fatalf("%s: HEAD = %d", path, head.StatusCode)
		}
		// RFC 9110 8.6: a HEAD reports the Content-Length its GET would have sent, which is what
		// makes HEAD worth issuing at all.
		if head.ContentLength != int64(len(content)) {
			t.Errorf("%s: HEAD Content-Length = %d, GET sent %d bytes", path, head.ContentLength, len(content))
		}
		for _, name := range []string{"Content-Language", "Content-Type", "ETag", "Vary"} {
			if head.Header.Get(name) != body.Header.Get(name) {
				t.Errorf("%s: HEAD %s = %q, GET %s = %q", path, name, head.Header.Get(name), name, body.Header.Get(name))
			}
		}
	}
}

func TestContinuationsSurviveATerminatingProxy(t *testing.T) {
	// A Service behind a proxy that terminates TLS reads http off the wire while publishing https,
	// so the scheme of an absolute continuation cannot be what decides its origin.
	for name, test := range map[string]struct {
		next string
		tls  bool
		want int
	}{
		"an https reference read over http": {next: "https://service.example/odp/offerings?cursor=c", want: http.StatusOK},
		"an https reference read over TLS":  {next: "https://service.example/odp/offerings?cursor=c", tls: true, want: http.StatusOK},
		"the default port written out":      {next: "https://service.example:443/odp/offerings?cursor=c", want: http.StatusOK},
		"a host in another case":            {next: "https://SERVICE.example/odp/offerings?cursor=c", want: http.StatusOK},
		"another port":                      {next: "https://service.example:8443/odp/offerings?cursor=c", want: http.StatusInternalServerError},
		"another host":                      {next: "https://evil.example/odp/offerings?cursor=c", want: http.StatusInternalServerError},
		"a downgrade to plain http":         {next: "http://service.example/odp/offerings?cursor=c", tls: true, want: http.StatusInternalServerError},
		"this request, spelled absolutely":  {next: "https://service.example/odp/offerings", want: http.StatusInternalServerError},
	} {
		runtime := serving(t, &answers{offerings: odp.Page[odp.Offering]{Next: test.next}})
		incoming := httptest.NewRequest(http.MethodGet, "/odp/offerings", nil)
		incoming.Host = "service.example"
		if test.tls {
			incoming.TLS = &tls.ConnectionState{}
		}
		recorder := httptest.NewRecorder()
		runtime.ServeHTTP(recorder, incoming)
		if recorder.Code != test.want {
			t.Errorf("%s: %d %s", name, recorder.Code, recorder.Body.String())
		}
	}
}

func TestACollectionCanBeIdentifiedAsOfferings(t *testing.T) {
	// "offerings" is a legal identifier, and the member route must not swallow the Collection whose
	// identifier happens to spell the segment that route ends with.
	runtime := serving(t, &answers{
		collection: &odp.Collection{Name: "Offerings"},
		members:    odp.Page[odp.Offering]{Items: []odp.Offering{anOffering("one")}},
	})
	collection := get(t, runtime, "/odp/collections/offerings", nil)
	if collection.Code != http.StatusOK {
		t.Fatalf("Collection named offerings = %d %s", collection.Code, collection.Body.String())
	}
	document := decodeObject(t, collection)
	if document["id"] != "offerings" || document["items"] != nil {
		t.Fatalf("served %s", collection.Body.String())
	}
	members := decodeObject(t, get(t, runtime, "/odp/collections/offerings/offerings", nil))
	if len(members["items"].([]any)) != 1 {
		t.Fatalf("members = %v", members)
	}
}

func TestNegotiationHeadersAreBounded(t *testing.T) {
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
	// Only the leading entries are read, so a header long enough to be an attack cannot buy work
	// proportional to its length. A preference buried past the bound simply does not apply.
	buried := strings.Repeat("xx,", 5_000) + "fr"
	response := get(t, runtime, "/odp/offerings", map[string]string{"Accept-Language": buried})
	if response.Code != http.StatusOK || response.Header().Get("Content-Language") != "en" {
		t.Fatalf("buried preference = %d %q", response.Code, response.Header().Get("Content-Language"))
	}
	// The same bound applies to Accept, which is read on the same path.
	acceptable := get(t, runtime, "/odp/offerings", map[string]string{
		"Accept": strings.Repeat("text/plain,", 5_000) + service.MediaType,
	})
	if acceptable.Code != http.StatusNotAcceptable {
		t.Fatalf("buried media type = %d", acceptable.Code)
	}
	if leading := get(t, runtime, "/odp/offerings", map[string]string{
		"Accept": service.MediaType + strings.Repeat(",text/plain", 5_000),
	}); leading.Code != http.StatusOK {
		t.Fatalf("leading media type = %d", leading.Code)
	}
}

func TestTitlesCarryNoLineBreakOfAnyKind(t *testing.T) {
	// A title travels into logs and displays. The C1 controls and the Unicode separators end a line
	// for a reader that the C0 controls alone would not have reached.
	noisy := serving(t, &answers{err: &service.Error{
		Code: "INVALID_REQUEST", Message: "one\u2028two\u0085three\u2029four\afive", Status: http.StatusBadRequest,
	}})
	title := decodeObject(t, get(t, noisy, "/odp/offerings", nil))["title"]
	if title != "one two three four five" {
		t.Fatalf("title = %q", title)
	}
}

func TestAuthExpandsMakesARepresentationPrivate(t *testing.T) {
	// REP-08 and PAG-04 let a document expand under authentication whatever its operation declares,
	// and a shared cache can only decline to reuse it if the response says so.
	expanding := serving(t, &answers{offerings: odp.Page[odp.Offering]{AuthExpands: true}})
	page := get(t, expanding, "/odp/offerings", nil)
	if page.Header().Get("Cache-Control") != "private" || !strings.Contains(page.Header().Get("Vary"), "Authorization") {
		t.Fatalf("expanding page headers = %v", page.Header())
	}
	resource := &odp.Offering{AuthExpands: true, ID: "one", Name: "One"}
	single := get(t, serving(t, &answers{offering: resource}), "/odp/offerings/one", nil)
	if single.Header().Get("Cache-Control") != "private" || !strings.Contains(single.Header().Get("Vary"), "Authorization") {
		t.Fatalf("expanding resource headers = %v", single.Header())
	}
	plain := get(t, serving(t, &answers{offerings: odp.Page[odp.Offering]{}}), "/odp/offerings", nil)
	if plain.Header().Get("Cache-Control") != "" {
		t.Fatalf("plain page headers = %v", plain.Header())
	}
}

func TestUnadvertisedRoutesAreAbsentWhateverTheMethod(t *testing.T) {
	minimal := service.Catalog{
		GetOffering: func(_ context.Context, _ string, _ service.CatalogRequest) (*odp.Offering, error) {
			return nil, nil
		},
		ListOfferings: func(_ context.Context, _ service.CatalogRequest) (odp.Page[odp.Offering], error) {
			return odp.Page[odp.Offering]{}, nil
		},
	}
	runtime := newService(t, minimal)
	for _, target := range []string{"/odp/collections", "/odp/collections/one", "/odp/collections/one/offerings"} {
		response := request(t, runtime, http.MethodPost, origin+target, nil, nil)
		if response.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d Allow %q", target, response.Code, response.Header().Get("Allow"))
		}
		if response.Header().Get("Allow") != "" {
			t.Errorf("POST %s advertised Allow %q for a route that is not there", target, response.Header().Get("Allow"))
		}
	}
}

func TestProblemResponsesAreNotStored(t *testing.T) {
	runtime := serving(t, &answers{})
	for _, target := range []string{"/odp/nowhere", "/odp/offerings/one"} {
		response := get(t, runtime, target, nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s = %d", target, response.Code)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q", target, response.Header().Get("Cache-Control"))
		}
	}
}

func TestStaticCatalogPagesAreStableBetweenRequests(t *testing.T) {
	runtime := staticService(t)
	first := get(t, runtime, "/odp/offerings?limit=1", nil)
	second := get(t, runtime, "/odp/offerings?limit=1", nil)
	// A continuation that carried the moment it was minted would change the page it appears in, and
	// with it the entity tag, making conditional retrieval useless on exactly the paginated
	// responses that most need it.
	if first.Body.String() != second.Body.String() {
		t.Fatalf("pages differ:\n%s\n%s", first.Body.String(), second.Body.String())
	}
	tag := first.Header().Get("ETag")
	if tag == "" || tag != second.Header().Get("ETag") {
		t.Fatalf("entity tags = %q and %q", tag, second.Header().Get("ETag"))
	}
	if revalidated := get(t, runtime, "/odp/offerings?limit=1", map[string]string{"If-None-Match": tag}); revalidated.Code != http.StatusNotModified {
		t.Fatalf("revalidated paginated page = %d", revalidated.Code)
	}
}

func TestStaticCatalogCursorsCanBeSharedBetweenProcesses(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	build := func() *service.Service {
		catalog, err := service.NewStaticCatalog(service.StaticCatalogOptions{
			ContinuationKey: key,
			Offerings: []odp.Offering{
				{ID: "one", Name: "One", ODPVersion: odp.Version},
				{ID: "two", Name: "Two", ODPVersion: odp.Version},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return newService(t, catalog)
	}
	next := decodeObject(t, get(t, build(), "/odp/offerings?limit=1", nil))["next"].(string)
	// A second process holding the same key redeems a cursor the first one issued.
	resumed := get(t, build(), next, nil)
	if resumed.Code != http.StatusOK {
		t.Fatalf("shared cursor = %d %s", resumed.Code, resumed.Body.String())
	}
	if item := decodeObject(t, resumed)["items"].([]any)[0].(map[string]any); item["id"] != "two" {
		t.Fatalf("resumed page = %v", item)
	}
	if _, err := service.NewStaticCatalog(service.StaticCatalogOptions{ContinuationKey: []byte("short")}); err == nil {
		t.Fatal("a key too short to sign with was accepted")
	}
}
