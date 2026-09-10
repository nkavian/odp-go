package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	odp "github.com/offering-protocol/odp-go"
	"github.com/offering-protocol/odp-go/internal/jsonvalue"
)

func (client *ServiceClient) supportingJSON(ctx context.Context, target, resourceClass, accept string, mediaTypes []string, maximumBytes int64, maximumDepth int, fallback time.Duration) (map[string]any, error) {
	current, err := url.Parse(target)
	if err != nil || current.Scheme != "https" || current.Host == "" {
		return nil, errors.New("ODP supporting document URL must use HTTPS")
	}
	origin, err := odp.DeriveServiceOrigin(current.String())
	if err != nil {
		return nil, err
	}
	key := cacheKey("anonymous:"+resourceClass+":"+accept, http.MethodGet, target, "", nil)
	record, cached, err := cachedRecord(ctx, client.supportingCache, key)
	if err != nil {
		return nil, err
	}
	// An entry whose stored representation came from another origin is not this document.
	if cached && record.FinalURL != "" {
		storedOrigin, storedErr := odp.DeriveServiceOrigin(record.FinalURL)
		if storedErr != nil || storedOrigin != origin {
			_ = client.supportingCache.Delete(ctx, key)
			cached = false
			record = CacheRecord{}
		}
	}
	if cached && time.Now().Before(record.ExpiresAt) {
		document, decodeErr := decodeSupportingJSON(record.Body)
		if decodeErr != nil {
			_ = client.supportingCache.Delete(ctx, key)
		}
		return document, decodeErr
	}
	for redirects := 0; ; redirects++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", accept)
		if cached {
			if record.ETag != "" {
				request.Header.Set("If-None-Match", record.ETag)
			}
			if record.LastModified != "" {
				request.Header.Set("If-Modified-Since", record.LastModified)
			}
		}
		response, err := client.supportingClient.Do(request)
		if err != nil {
			return nil, err
		}
		if redirectStatus(response.StatusCode) {
			_ = response.Body.Close()
			if redirects >= client.maxRedirects {
				return nil, errors.New("ODP supporting document exceeded its redirect limit")
			}
			next, parseErr := current.Parse(response.Header.Get("Location"))
			if parseErr != nil || next.Scheme != "https" || next.Host == "" {
				return nil, errors.New("ODP supporting document redirect must use HTTPS")
			}
			// A redirect for a supporting document keeps the scheme, host and port it started on,
			// so a schema or OpenAPI URL cannot be pointed at another origin after the fact.
			nextOrigin, originErr := odp.DeriveServiceOrigin(next.String())
			if originErr != nil || nextOrigin != origin {
				return nil, errors.New("ODP supporting document redirect changed origin")
			}
			current = next
			continue
		}
		if response.StatusCode == http.StatusNotModified && cached {
			_ = response.Body.Close()
			if echoed := response.Header.Get("ETag"); echoed != "" && echoed != record.ETag {
				_ = client.supportingCache.Delete(ctx, key)
				return nil, errors.New("ODP supporting document 304 confirmed a validator the cache does not hold")
			}
			if !supportedVary(response.Header.Values("Vary")) {
				if err := client.supportingCache.Delete(ctx, key); err != nil {
					return nil, err
				}
				return decodeSupportingJSON(record.Body)
			}
			if _, noStore := cacheDirectives(response.Header.Get("Cache-Control"))["no-store"]; noStore {
				if err := client.supportingCache.Delete(ctx, key); err != nil {
					return nil, err
				}
				return decodeSupportingJSON(record.Body)
			}
			now := time.Now()
			if hasFreshnessDirective(response.Header) {
				record.ExpiresAt = expiry(response.Header, fallback, now)
			} else {
				lifetime := time.Duration(0)
				if !record.StoredAt.IsZero() {
					lifetime = record.ExpiresAt.Sub(record.StoredAt)
				}
				if lifetime < 0 {
					lifetime = 0
				}
				record.ExpiresAt = now.Add(lifetime)
			}
			if value := response.Header.Get("ETag"); value != "" {
				record.ETag = value
			}
			if value := response.Header.Get("Last-Modified"); value != "" {
				record.LastModified = value
			}
			record.FinalURL = current.String()
			record.StoredAt = now
			if err := client.supportingCache.Set(ctx, key, record); err != nil {
				return nil, err
			}
			return decodeSupportingJSON(record.Body)
		}
		if response.StatusCode < 200 || response.StatusCode > 299 {
			_ = response.Body.Close()
			return nil, fmt.Errorf("ODP supporting document returned HTTP %d", response.StatusCode)
		}
		mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if mediaErr != nil || !slices.Contains(mediaTypes, strings.ToLower(mediaType)) {
			_ = response.Body.Close()
			return nil, errors.New("ODP supporting document has an unsupported media type")
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
		_ = response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if int64(len(body)) > maximumBytes {
			return nil, fmt.Errorf("%w: ODP supporting document exceeds its byte limit", ErrResponseLimitExceeded)
		}
		if jsonvalue.Depth(body) > maximumDepth {
			return nil, fmt.Errorf("%w: ODP supporting document exceeds its nesting-depth limit", ErrResponseLimitExceeded)
		}
		document, err := decodeSupportingJSON(body)
		if err != nil {
			return nil, err
		}
		if cacheable(http.MethodGet, response.Header, fallback) {
			now := time.Now()
			record = CacheRecord{Body: body, ETag: response.Header.Get("ETag"), ExpiresAt: expiry(response.Header, fallback, now), FinalURL: current.String(), LastModified: response.Header.Get("Last-Modified"), Status: response.StatusCode, StoredAt: now}
			if err := client.supportingCache.Set(ctx, key, record); err != nil {
				return nil, err
			}
		} else if err := client.supportingCache.Delete(ctx, key); err != nil {
			return nil, err
		}
		return document, nil
	}
}

func decodeSupportingJSON(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil || document == nil {
		return nil, errors.New("ODP supporting document must be a JSON object")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("ODP supporting document must contain one JSON value")
	}
	return document, nil
}
