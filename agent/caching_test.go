package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// countingCache records what a client asks of its cache and can be made to fail on demand, so the
// paths that report a cache failure rather than swallowing it are reachable from a test.
type countingCache struct {
	deleteErr error
	deletes   int
	records   map[string]CacheRecord
	setErr    error
	sets      int
}

func newCountingCache() *countingCache {
	return &countingCache{records: map[string]CacheRecord{}}
}

func (cache *countingCache) Delete(_ context.Context, key string) error {
	cache.deletes++
	if cache.deleteErr != nil {
		return cache.deleteErr
	}
	delete(cache.records, key)
	return nil
}

func (cache *countingCache) Get(_ context.Context, key string) (CacheRecord, bool, error) {
	record, found := cache.records[key]
	return record, found, nil
}

func (cache *countingCache) Set(_ context.Context, key string, record CacheRecord) error {
	cache.sets++
	if cache.setErr != nil {
		return cache.setErr
	}
	cache.records[key] = record
	return nil
}

const supportingTarget = "https://schemas.example/root.json"

const supportingBody = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}`

func supportingKey() string {
	return cacheKey("anonymous:attribute-schema:application/schema+json", http.MethodGet, supportingTarget, "", nil)
}

// fetchSupporting asks for one supporting document through the client's real caching path.
func fetchSupporting(client *ServiceClient) (map[string]any, error) {
	return client.supportingJSON(context.Background(), supportingTarget, "attribute-schema",
		"application/schema+json", []string{"application/schema+json"}, 65_536, maximumResourceDepth, time.Minute)
}

func supportingClientWith(t *testing.T, cache Cache, handler func(*http.Request) (*http.Response, error)) *ServiceClient {
	t.Helper()
	client, err := NewServiceClient(ServiceClientOptions{
		Cache:                cache,
		ServiceURL:           "https://service.example",
		SupportingHTTPClient: &http.Client{Transport: roundTripFunc(handler)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func notModified(headers http.Header) *http.Response {
	if headers == nil {
		headers = http.Header{}
	}
	return &http.Response{Body: io.NopCloser(strings.NewReader("")), Header: headers, StatusCode: http.StatusNotModified}
}

func TestSupportingRevalidationRefreshesTheStoredLifetime(t *testing.T) {
	stored := time.Now().Add(-2 * time.Minute)
	for name, test := range map[string]struct {
		headers  http.Header
		record   CacheRecord
		wantKept bool
		within   func(time.Time) bool
	}{
		"a freshness directive replaces the lifetime": {
			headers:  http.Header{"Cache-Control": {"max-age=600"}},
			record:   CacheRecord{Body: []byte(supportingBody), ETag: `"v1"`, ExpiresAt: stored.Add(time.Minute), StoredAt: stored},
			wantKept: true,
			within:   func(value time.Time) bool { return value.After(time.Now().Add(9 * time.Minute)) },
		},
		"no directive reuses the lifetime the entry had": {
			headers: http.Header{},
			record: CacheRecord{
				Body: []byte(supportingBody), ETag: `"v1"`,
				ExpiresAt: time.Now().Add(-6 * time.Minute), StoredAt: time.Now().Add(-10 * time.Minute),
			},
			wantKept: true,
			within: func(value time.Time) bool {
				return value.After(time.Now().Add(3*time.Minute)) && value.Before(time.Now().Add(5*time.Minute))
			},
		},
		// A stored entry whose expiry precedes the moment it was stored has no lifetime to carry
		// forward, and must not be revived by a negative one.
		"a lifetime that ran backwards becomes none": {
			headers:  http.Header{},
			record:   CacheRecord{Body: []byte(supportingBody), ETag: `"v1"`, ExpiresAt: stored.Add(-time.Minute), StoredAt: stored},
			wantKept: true,
			within:   func(value time.Time) bool { return !value.After(time.Now()) },
		},
		// A revalidation that says no-store evicts the entry, but the body it just confirmed is
		// still the answer to this request.
		"no-store evicts the entry it just confirmed": {
			headers:  http.Header{"Cache-Control": {"no-store"}},
			record:   CacheRecord{Body: []byte(supportingBody), ETag: `"v1"`, ExpiresAt: stored, StoredAt: stored},
			wantKept: false,
		},
	} {
		cache := newCountingCache()
		cache.records[supportingKey()] = test.record
		var conditional string
		client := supportingClientWith(t, cache, func(request *http.Request) (*http.Response, error) {
			conditional = request.Header.Get("If-None-Match")
			return notModified(test.headers), nil
		})
		document, err := fetchSupporting(client)
		if err != nil || document["type"] != "object" {
			t.Errorf("%s: document = %v, err = %v", name, document, err)
			continue
		}
		if conditional != `"v1"` {
			t.Errorf("%s: If-None-Match = %q", name, conditional)
		}
		refreshed, kept := cache.records[supportingKey()]
		if kept != test.wantKept {
			t.Errorf("%s: entry kept = %t", name, kept)
			continue
		}
		if test.within != nil && !test.within(refreshed.ExpiresAt) {
			t.Errorf("%s: refreshed expiry = %v", name, refreshed.ExpiresAt)
		}
	}
}

func TestSupportingRevalidationSendsEveryValidatorItHolds(t *testing.T) {
	cache := newCountingCache()
	cache.records[supportingKey()] = CacheRecord{
		Body: []byte(supportingBody), ETag: `"v1"`, ExpiresAt: time.Now().Add(-time.Minute),
		LastModified: "Wed, 21 Oct 2026 07:28:00 GMT", StoredAt: time.Now().Add(-2 * time.Minute),
	}
	var modifiedSince string
	client := supportingClientWith(t, cache, func(request *http.Request) (*http.Response, error) {
		modifiedSince = request.Header.Get("If-Modified-Since")
		return notModified(nil), nil
	})
	if _, err := fetchSupporting(client); err != nil {
		t.Fatal(err)
	}
	if modifiedSince != "Wed, 21 Oct 2026 07:28:00 GMT" {
		t.Fatalf("If-Modified-Since = %q", modifiedSince)
	}
}

func TestSupportingRevalidationValidatesAndUpdatesMetadata(t *testing.T) {
	for name, test := range map[string]struct {
		headers  http.Header
		wantErr  string
		wantKept bool
	}{
		"a mismatched validator": {
			headers: http.Header{"Etag": {`"v2"`}}, wantErr: "does not hold",
		},
		"an unsupported Vary": {
			headers: http.Header{"Vary": {"X-Tenant"}},
		},
		"updated metadata": {
			headers: http.Header{"Etag": {`"v1"`}, "Last-Modified": {"Thu, 22 Oct 2026 07:28:00 GMT"}}, wantKept: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			cache := newCountingCache()
			cache.records[supportingKey()] = CacheRecord{
				Body: []byte(supportingBody), ETag: `"v1"`, ExpiresAt: time.Now().Add(-time.Minute),
				FinalURL: supportingTarget, LastModified: "Wed, 21 Oct 2026 07:28:00 GMT", StoredAt: time.Now().Add(-2 * time.Minute),
			}
			client := supportingClientWith(t, cache, func(*http.Request) (*http.Response, error) {
				return notModified(test.headers), nil
			})
			_, err := fetchSupporting(client)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("error = %v", err)
			}
			record, kept := cache.records[supportingKey()]
			if kept != test.wantKept {
				t.Fatalf("entry kept = %t", kept)
			}
			if test.wantKept && (record.LastModified != "Thu, 22 Oct 2026 07:28:00 GMT" || record.FinalURL != supportingTarget) {
				t.Fatalf("record = %#v", record)
			}
		})
	}
}

func TestSupportingCacheFailuresAreReported(t *testing.T) {
	failure := errors.New("cache is unavailable")
	fresh := CacheRecord{Body: []byte(supportingBody), ETag: `"v1"`, ExpiresAt: time.Now().Add(-time.Minute), StoredAt: time.Now().Add(-2 * time.Minute)}
	for name, test := range map[string]struct {
		handler func(*http.Request) (*http.Response, error)
		record  *CacheRecord
		set     bool
	}{
		"storing a revalidated entry": {
			handler: func(*http.Request) (*http.Response, error) { return notModified(nil), nil },
			record:  &fresh, set: true,
		},
		"evicting after a no-store revalidation": {
			handler: func(*http.Request) (*http.Response, error) {
				return notModified(http.Header{"Cache-Control": {"no-store"}}), nil
			},
			record: &fresh,
		},
		"storing a fresh response": {
			handler: func(request *http.Request) (*http.Response, error) {
				return jsonResponse(request, supportingBody, "application/schema+json"), nil
			},
			set: true,
		},
		"evicting a response that may not be stored": {
			handler: func(request *http.Request) (*http.Response, error) {
				response := jsonResponse(request, supportingBody, "application/schema+json")
				response.Header.Set("Cache-Control", "no-store")
				return response, nil
			},
		},
	} {
		cache := newCountingCache()
		if test.set {
			cache.setErr = failure
		} else {
			cache.deleteErr = failure
		}
		if test.record != nil {
			cache.records[supportingKey()] = *test.record
		}
		client := supportingClientWith(t, cache, test.handler)
		if _, err := fetchSupporting(client); !errors.Is(err, failure) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSupportingRedirectsAreFollowedAndBounded(t *testing.T) {
	hops := 0
	client := supportingClientWith(t, newCountingCache(), func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/root.json" {
			hops++
			return &http.Response{
				Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": {"/moved.json"}},
				StatusCode: http.StatusFound,
			}, nil
		}
		return jsonResponse(request, supportingBody, "application/schema+json"), nil
	})
	if _, err := fetchSupporting(client); err != nil {
		t.Fatalf("one same-origin redirect: %v", err)
	}
	if hops != 1 {
		t.Fatalf("hops = %d", hops)
	}

	for name, test := range map[string]struct {
		location string
		wantErr  string
	}{
		"an endless chain":           {location: "/root.json", wantErr: "redirect limit"},
		"a plain-HTTP hop":           {location: "http://schemas.example/root.json", wantErr: "must use HTTPS"},
		"a scheme that is not a URL": {location: "mailto:someone@schemas.example", wantErr: "must use HTTPS"},
		"another origin":             {location: "https://elsewhere.example/root.json", wantErr: "changed origin"},
	} {
		looping := supportingClientWith(t, newCountingCache(), func(*http.Request) (*http.Response, error) {
			return &http.Response{
				Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": {test.location}},
				StatusCode: http.StatusFound,
			}, nil
		})
		if _, err := fetchSupporting(looping); err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Errorf("%s: err = %v, want %q", name, err, test.wantErr)
		}
	}
}

func TestAFreshButUnreadableEntryIsEvicted(t *testing.T) {
	cache := newCountingCache()
	cache.records[supportingKey()] = CacheRecord{Body: []byte("{not json"), ExpiresAt: time.Now().Add(time.Hour), StoredAt: time.Now()}
	client := supportingClientWith(t, cache, func(*http.Request) (*http.Response, error) {
		t.Error("a fresh entry was revalidated")
		return nil, errors.New("unreachable")
	})
	if _, err := fetchSupporting(client); err == nil {
		t.Fatal("an undecodable cached body was served")
	}
	if _, kept := cache.records[supportingKey()]; kept {
		t.Fatal("an undecodable entry was kept")
	}
}

func TestAnUnreadableSupportingBodyIsReported(t *testing.T) {
	client := supportingClientWith(t, newCountingCache(), func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			Body: io.NopCloser(failingReader{}), Header: http.Header{"Content-Type": {"application/schema+json"}},
			Request: request, StatusCode: http.StatusOK,
		}, nil
	})
	if _, err := fetchSupporting(client); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }
