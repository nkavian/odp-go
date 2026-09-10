package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func odpResponse(status int, body string, headers map[string]string) *http.Response {
	header := http.Header{}
	for name, value := range headers {
		header.Set(name, value)
	}
	if status >= 200 && status <= 299 && header.Get("Content-Type") == "" {
		header.Set("Content-Type", mediaTypeODP)
	}
	return &http.Response{Body: io.NopCloser(strings.NewReader(body)), Header: header, StatusCode: status}
}

const transportDocument = `{"description":"Catalog","http":{"endpoint_base":"/odp"},"language":"en","localizations":["en"],"name":"Example","odp_version":"1.0","operations":[{"authentication":"not-required","name":"get-offering"},{"authentication":"not-required","name":"list-offerings"}]}`

// fetch drives one request through the transport with a stub round tripper.
func fetch(t *testing.T, handler func(*http.Request) (*http.Response, error), cache Cache, key string, fallback time.Duration, validate func([]byte) error) (responseData, error) {
	t.Helper()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     roundTripFunc(handler),
	}
	return request(t.Context(), client, http.MethodGet, "https://service.example/.well-known/odp", nil,
		"en", 2, maximumDocumentBytes, cache, key, fallback, validate)
}

func TestRedirectsAreBoundedAndOriginLocked(t *testing.T) {
	cases := map[string]struct {
		handler func(*http.Request) (*http.Response, error)
		wantErr string
	}{
		"missing Location": {
			handler: func(*http.Request) (*http.Response, error) {
				return odpResponse(http.StatusFound, "", nil), nil
			},
			wantErr: "omitted Location",
		},
		// net/http parses Location itself before the response reaches this code, so the
		// transport's own parse guard is defence in depth; either way the redirect is refused.
		"unparseable Location": {
			handler: func(*http.Request) (*http.Response, error) {
				return odpResponse(http.StatusFound, "", map[string]string{"Location": "://"}), nil
			},
			wantErr: "Location",
		},
		"another origin": {
			handler: func(*http.Request) (*http.Response, error) {
				return odpResponse(http.StatusFound, "", map[string]string{"Location": "https://elsewhere.example/odp"}), nil
			},
			wantErr: "changed Service origin",
		},
		"too many hops": {
			handler: func(request *http.Request) (*http.Response, error) {
				return odpResponse(http.StatusFound, "", map[string]string{"Location": request.URL.Path + "/again"}), nil
			},
			wantErr: "redirect limit",
		},
	}
	for name, test := range cases {
		if _, err := fetch(t, test.handler, nil, "", time.Hour, nil); err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Errorf("%s: error = %v, want %q", name, err, test.wantErr)
		}
	}
}

func TestSameOriginRedirectIsFollowed(t *testing.T) {
	hops := 0
	result, err := fetch(t, func(request *http.Request) (*http.Response, error) {
		hops++
		if request.URL.Path == "/.well-known/odp" {
			return odpResponse(http.StatusMovedPermanently, "", map[string]string{"Location": "/moved"}), nil
		}
		return odpResponse(http.StatusOK, transportDocument, nil), nil
	}, nil, "", time.Hour, nil)
	if err != nil || hops != 2 {
		t.Fatalf("redirect = %d hops, %v", hops, err)
	}
	if !strings.HasSuffix(result.finalURL, "/moved") {
		t.Fatalf("final URL = %q", result.finalURL)
	}
}

func TestRedirectsRewriteTheMethodTheWayTheStatusRequires(t *testing.T) {
	for _, test := range []struct {
		status int
		want   string
	}{
		{status: http.StatusMovedPermanently, want: http.MethodGet},
		{status: http.StatusFound, want: http.MethodGet},
		{status: http.StatusSeeOther, want: http.MethodGet},
		{status: http.StatusTemporaryRedirect, want: http.MethodPost},
		{status: http.StatusPermanentRedirect, want: http.MethodPost},
	} {
		var followed string
		client := &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/odp/offerings/search" {
					return odpResponse(test.status, "", map[string]string{"Location": "/odp/moved"}), nil
				}
				followed = request.Method
				return odpResponse(http.StatusOK, `{"odp_version":"1.0","items":[]}`, nil), nil
			}),
		}
		_, err := request(t.Context(), client, http.MethodPost, "https://service.example/odp/offerings/search",
			[]byte(`{"odp_version":"1.0","query":"x"}`), "", 2, maximumResourceBytes, nil, "", 0, nil)
		if err != nil {
			t.Fatalf("status %d: %v", test.status, err)
		}
		if followed != test.want {
			t.Errorf("status %d followed with %s, want %s", test.status, followed, test.want)
		}
	}
}

func TestStoredEntriesAreDiscardedWhenTheyNoLongerDescribeTheResource(t *testing.T) {
	key := "entry"
	cache := NewMemoryCache()
	if err := cache.Set(t.Context(), key, CacheRecord{
		Body: []byte(transportDocument), ExpiresAt: time.Now().Add(time.Hour),
		FinalURL: "https://elsewhere.example/odp", Status: http.StatusOK, StoredAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	fetched := 0
	// A stored final URL off the requested origin is not this resource, whoever put it there.
	if _, err := fetch(t, func(*http.Request) (*http.Response, error) {
		fetched++
		return odpResponse(http.StatusOK, transportDocument, nil), nil
	}, cache, key, time.Hour, nil); err != nil {
		t.Fatal(err)
	}
	if fetched != 1 {
		t.Fatalf("fetches = %d, want the foreign entry discarded", fetched)
	}

	// An unparseable stored final URL is treated the same way.
	if err := cache.Set(t.Context(), key, CacheRecord{
		Body: []byte(transportDocument), ExpiresAt: time.Now().Add(time.Hour),
		FinalURL: "://", Status: http.StatusOK, StoredAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	fetched = 0
	if _, err := fetch(t, func(*http.Request) (*http.Response, error) {
		fetched++
		return odpResponse(http.StatusOK, transportDocument, nil), nil
	}, cache, key, time.Hour, nil); err != nil {
		t.Fatal(err)
	}
	if fetched != 1 {
		t.Fatalf("fetches = %d, want the unusable entry discarded", fetched)
	}
}

func TestAFreshEntryThatNoLongerValidatesIsDropped(t *testing.T) {
	key := "entry"
	cache := NewMemoryCache()
	if err := cache.Set(t.Context(), key, CacheRecord{
		Body: []byte(`{"odp_version":"1.0"}`), ExpiresAt: time.Now().Add(time.Hour),
		FinalURL: "https://service.example/.well-known/odp", Status: http.StatusOK, StoredAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("no longer valid")
	if _, err := fetch(t, func(*http.Request) (*http.Response, error) {
		return odpResponse(http.StatusOK, transportDocument, nil), nil
	}, cache, key, time.Hour, func([]byte) error { return rejected }); !errors.Is(err, rejected) {
		t.Fatalf("validation error = %v", err)
	}
	if _, cached, err := cache.Get(t.Context(), key); cached || err != nil {
		t.Fatalf("entry still held = %v, %v", cached, err)
	}
}

func TestRevalidationHonoursNoStore(t *testing.T) {
	key := "entry"
	cache := NewMemoryCache()
	if err := cache.Set(t.Context(), key, CacheRecord{
		Body: []byte(transportDocument), ETag: `"v1"`, ExpiresAt: time.Now().Add(-time.Minute),
		FinalURL: "https://service.example/.well-known/odp", Status: http.StatusOK, StoredAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	result, err := fetch(t, func(*http.Request) (*http.Response, error) {
		return odpResponse(http.StatusNotModified, "", map[string]string{"Cache-Control": "no-store", "ETag": `"v1"`}), nil
	}, cache, key, time.Hour, nil)
	if err != nil || result.freshness != FreshnessRevalidated {
		t.Fatalf("revalidation = %q, %v", result.freshness, err)
	}
	// The Service asked for the representation not to be kept, so the entry goes even though the
	// body it confirmed is still what the caller receives.
	if _, cached, err := cache.Get(t.Context(), key); cached || err != nil {
		t.Fatalf("entry retained after no-store = %v, %v", cached, err)
	}
}

func TestRevalidationRefreshesTheStoredValidators(t *testing.T) {
	key := "entry"
	cache := NewMemoryCache()
	if err := cache.Set(t.Context(), key, CacheRecord{
		Body: []byte(transportDocument), ETag: `"v1"`, ExpiresAt: time.Now().Add(-time.Minute),
		FinalURL: "https://service.example/.well-known/odp", LastModified: "Mon, 01 Jan 2024 00:00:00 GMT",
		Status: http.StatusOK, StoredAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fetch(t, func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("If-None-Match") != `"v1"` || request.Header.Get("If-Modified-Since") == "" {
			return nil, errors.New("conditional headers were not sent")
		}
		return odpResponse(http.StatusNotModified, "", map[string]string{
			"Cache-Control": "max-age=600", "ETag": `"v1"`, "Last-Modified": "Tue, 02 Jan 2024 00:00:00 GMT",
		}), nil
	}, cache, key, time.Hour, nil); err != nil {
		t.Fatal(err)
	}
	record, cached, err := cache.Get(t.Context(), key)
	if err != nil || !cached {
		t.Fatalf("entry = %v, %v", cached, err)
	}
	if record.LastModified != "Tue, 02 Jan 2024 00:00:00 GMT" || !record.ExpiresAt.After(time.Now()) {
		t.Fatalf("record = %#v", record)
	}
}

func TestResponsesAreRejectedOnTheirWireShape(t *testing.T) {
	cases := map[string]struct {
		handler func(*http.Request) (*http.Response, error)
		wantErr string
	}{
		"wrong media type": {
			handler: func(*http.Request) (*http.Response, error) {
				return odpResponse(http.StatusOK, transportDocument, map[string]string{"Content-Type": "application/json"}), nil
			},
			wantErr: "unsupported media type",
		},
		"absent media type": {
			handler: func(*http.Request) (*http.Response, error) {
				return &http.Response{Body: io.NopCloser(strings.NewReader(transportDocument)), Header: http.Header{}, StatusCode: http.StatusOK}, nil
			},
			wantErr: "unsupported media type",
		},
		"not UTF-8": {
			handler: func(*http.Request) (*http.Response, error) {
				return odpResponse(http.StatusOK, string([]byte{0xff, 0xfe}), nil), nil
			},
			wantErr: "UTF-8",
		},
		"declared over the limit": {
			handler: func(*http.Request) (*http.Response, error) {
				response := odpResponse(http.StatusOK, transportDocument, nil)
				response.ContentLength = maximumDocumentBytes + 1
				return response, nil
			},
			wantErr: "byte limit",
		},
		"streamed over the limit": {
			handler: func(*http.Request) (*http.Response, error) {
				return odpResponse(http.StatusOK, strings.Repeat("x", maximumDocumentBytes+1), nil), nil
			},
			wantErr: "byte limit",
		},
	}
	for name, test := range cases {
		_, err := fetch(t, test.handler, nil, "", time.Hour, nil)
		if err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Errorf("%s: error = %v, want %q", name, err, test.wantErr)
		}
	}
}

func TestTransportFailuresReachTheCaller(t *testing.T) {
	refused := errors.New("connection refused")
	if _, err := fetch(t, func(*http.Request) (*http.Response, error) { return nil, refused }, nil, "", time.Hour, nil); !errors.Is(err, refused) {
		t.Fatalf("transport error = %v", err)
	}
}

// failingCache reports an error from whichever operation it is told to fail.
type failingCache struct {
	failure  error
	onDelete bool
	onGet    bool
	onSet    bool
	delegate Cache
}

func (cache failingCache) Delete(ctx context.Context, key string) error {
	if cache.onDelete {
		return cache.failure
	}
	return cache.delegate.Delete(ctx, key)
}

func (cache failingCache) Get(ctx context.Context, key string) (CacheRecord, bool, error) {
	if cache.onGet {
		return CacheRecord{}, false, cache.failure
	}
	return cache.delegate.Get(ctx, key)
}

func (cache failingCache) Set(ctx context.Context, key string, record CacheRecord) error {
	if cache.onSet {
		return cache.failure
	}
	return cache.delegate.Set(ctx, key, record)
}

func TestCacheFailuresAreReportedRatherThanSwallowed(t *testing.T) {
	failure := errors.New("cache unavailable")
	document := func(*http.Request) (*http.Response, error) {
		return odpResponse(http.StatusOK, transportDocument, nil), nil
	}
	if _, err := fetch(t, document, failingCache{delegate: NewMemoryCache(), failure: failure, onGet: true}, "entry", time.Hour, nil); !errors.Is(err, failure) {
		t.Errorf("read failure = %v", err)
	}
	if _, err := fetch(t, document, failingCache{delegate: NewMemoryCache(), failure: failure, onSet: true}, "entry", time.Hour, nil); !errors.Is(err, failure) {
		t.Errorf("write failure = %v", err)
	}
	// A response that must not be stored still has to evict whatever was there before.
	noStore := func(*http.Request) (*http.Response, error) {
		return odpResponse(http.StatusOK, transportDocument, map[string]string{"Cache-Control": "no-store"}), nil
	}
	if _, err := fetch(t, noStore, failingCache{delegate: NewMemoryCache(), failure: failure, onDelete: true}, "entry", time.Hour, nil); !errors.Is(err, failure) {
		t.Errorf("eviction failure = %v", err)
	}
}

func TestProblemResponsesCarryTheirDetails(t *testing.T) {
	problem := `{"code":"RATE_LIMITED","status":429,"title":"Too Many Requests","type":"https://offeringprotocol.org/problems/rate-limited"}`
	_, err := fetch(t, func(*http.Request) (*http.Response, error) {
		return odpResponse(http.StatusTooManyRequests, problem, map[string]string{
			"Content-Type": mediaTypeProblem, "Retry-After": "30",
		}), nil
	}, nil, "", time.Hour, nil)
	var failure *RequestError
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v", err)
	}
	if failure.Code != "RATE_LIMITED" || !failure.Retryable || failure.Header.Get("Retry-After") != "30" {
		t.Fatalf("failure = %#v", failure)
	}

	// A body that is not Problem Details still produces a typed failure, just without one.
	_, err = fetch(t, func(*http.Request) (*http.Response, error) {
		return odpResponse(http.StatusBadRequest, "not json", map[string]string{"Content-Type": mediaTypeProblem}), nil
	}, nil, "", time.Hour, nil)
	if !errors.As(err, &failure) || failure.Code != "HTTP_ERROR" || failure.Problem != nil {
		t.Fatalf("unparseable problem = %#v, %v", failure, err)
	}

	// Problem Details nested past the limit are not parsed at all.
	deep := strings.Repeat(`{"a":`, 40) + "1" + strings.Repeat("}", 40)
	_, err = fetch(t, func(*http.Request) (*http.Response, error) {
		return odpResponse(http.StatusBadRequest, deep, map[string]string{"Content-Type": mediaTypeProblem}), nil
	}, nil, "", time.Hour, nil)
	if !errors.As(err, &failure) || failure.Problem != nil {
		t.Fatalf("deep problem = %#v, %v", failure, err)
	}
}

func TestProblemBodiesAreHeldToTheirOwnByteLimit(t *testing.T) {
	oversized := fmt.Sprintf(`{"code":"INVALID_REQUEST","detail":"%s","status":400,"title":"Bad","type":"https://offeringprotocol.org/problems/invalid-request"}`,
		strings.Repeat("x", maximumProblemBytes))
	_, err := fetch(t, func(*http.Request) (*http.Response, error) {
		return odpResponse(http.StatusBadRequest, oversized, map[string]string{"Content-Type": mediaTypeProblem}), nil
	}, nil, "", time.Hour, nil)
	if !errors.Is(err, ErrResponseLimitExceeded) {
		t.Fatalf("oversized problem error = %v", err)
	}
}
