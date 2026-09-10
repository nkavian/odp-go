package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	odp "github.com/offering-protocol/odp-go"
	"github.com/offering-protocol/odp-go/internal/jsonvalue"
)

const (
	maximumCapabilityPages = 16
	maximumFilters         = 1024
	maximumSorts           = 128
)

func (client *ServiceClient) GetCollectionSearchCapabilities(ctx context.Context, id string) (SearchCapabilityCatalog, error) {
	collection, err := client.GetCollection(ctx, id, odp.RepresentationFull)
	if err != nil {
		return SearchCapabilityCatalog{}, err
	}
	return client.resolveSearchCapabilities(ctx, &collection)
}

func (client *ServiceClient) GetOfferingSearchCapabilities(ctx context.Context, collectionID string) (SearchCapabilityCatalog, error) {
	var collection *odp.Collection
	if collectionID != "" {
		value, err := client.GetCollection(ctx, collectionID, odp.RepresentationFull)
		if err != nil {
			return SearchCapabilityCatalog{}, err
		}
		collection = &value
	}
	return client.resolveSearchCapabilities(ctx, collection)
}

func (client *ServiceClient) resolveSearchCapabilities(ctx context.Context, collection *odp.Collection) (SearchCapabilityCatalog, error) {
	inspection, err := client.Inspect(ctx)
	if err != nil {
		return SearchCapabilityCatalog{}, err
	}
	result := SearchCapabilityCatalog{Filters: map[string]odp.FilterDefinition{}, Sorts: map[string]ResolvedSortDefinition{}}
	if !supports(inspection.Document, odp.OperationSearchOfferings) {
		if collection != nil && collection.SearchCapabilities != nil {
			result.Issues = append(result.Issues, CapabilityIssue{Kind: CapabilityKindFilters, Message: "Collection search capabilities require the search-offerings operation.", Scope: CapabilityScopeCollection})
		}
		return result, nil
	}
	sorts := map[string]odp.SortDefinition{}
	sortScopes := map[string]CapabilityScope{}
	for _, source := range []struct {
		capabilities *odp.SearchCapabilities
		scope        CapabilityScope
	}{{inspection.Document.SearchCapabilities, CapabilityScopeService}, {collectionCapabilities(collection), CapabilityScopeCollection}} {
		if source.capabilities == nil {
			continue
		}
		client.addFilters(ctx, &result, source.scope, source.capabilities.Filters)
		client.addSorts(ctx, &result, sorts, sortScopes, source.scope, source.capabilities.Sorts)
	}
	// A Go map iterates in an unspecified order, and the issues below reach the caller.
	for _, id := range sortedKeys(sorts) {
		sort := sorts[id]
		resolved := ResolvedSortDefinition{SortDefinition: sort}
		missing := false
		for _, key := range sort.Keys {
			filter, found := result.Filters[key.FilterID]
			if !found {
				missing = true
				break
			}
			resolved.Filters = append(resolved.Filters, filter)
		}
		if missing {
			result.Issues = append(result.Issues, CapabilityIssue{Kind: CapabilityKindSorts, Message: fmt.Sprintf("Sort %s references an unavailable filter.", id), Scope: sortScopes[id]})
			continue
		}
		result.Sorts[id] = resolved
	}
	return result, nil
}

func collectionCapabilities(collection *odp.Collection) *odp.SearchCapabilities {
	if collection == nil {
		return nil
	}
	return collection.SearchCapabilities
}

func (client *ServiceClient) addFilters(ctx context.Context, result *SearchCapabilityCatalog, scope CapabilityScope, source *odp.FilterCapabilitySource) {
	if source == nil {
		return
	}
	values := append([]odp.FilterDefinition(nil), source.Inline...)
	if source.Linked != nil {
		loaded, err := client.loadFilterPages(ctx, source.Linked.Href, maximumFilters-len(result.Filters))
		if err != nil {
			result.Issues = append(result.Issues, CapabilityIssue{Kind: CapabilityKindFilters, Message: err.Error(), Scope: scope})
			return
		}
		values = loaded
	}
	identifiers := make([]string, len(values))
	for index, value := range values {
		identifiers[index] = value.ID
	}
	// FLT-55: a source is atomic. One that repeats an identifier within itself is not usable at
	// all, so nothing from it is exposed — unlike a clash between two sources, handled below.
	if repeated, found := firstRepeated(identifiers); found {
		result.Issues = append(result.Issues, CapabilityIssue{Kind: CapabilityKindFilters, Message: "Source repeats filter " + repeated + ".", Scope: scope})
		return
	}
	if len(result.Filters)+len(values) > maximumFilters {
		result.Issues = append(result.Issues, CapabilityIssue{Kind: CapabilityKindFilters, Message: "Effective filters exceed their limit.", Scope: scope})
		return
	}
	// FLT-65: an identifier two sources both publish is quarantined on both sides.
	conflicts := map[string]bool{}
	for _, value := range values {
		if _, found := result.Filters[value.ID]; found {
			conflicts[value.ID] = true
		}
	}
	for id := range conflicts {
		delete(result.Filters, id)
	}
	for _, value := range values {
		if !conflicts[value.ID] {
			result.Filters[value.ID] = value
		}
	}
	if len(conflicts) != 0 {
		result.Issues = append(result.Issues, CapabilityIssue{Kind: CapabilityKindFilters, Message: "Duplicate filters: " + duplicateNames(conflicts), Scope: scope})
	}
}

func (client *ServiceClient) addSorts(ctx context.Context, result *SearchCapabilityCatalog, target map[string]odp.SortDefinition, scopes map[string]CapabilityScope, scope CapabilityScope, source *odp.SortCapabilitySource) {
	if source == nil {
		return
	}
	values := append([]odp.SortDefinition(nil), source.Inline...)
	if source.Linked != nil {
		loaded, err := client.loadSortPages(ctx, source.Linked.Href, maximumSorts-len(target))
		if err != nil {
			result.Issues = append(result.Issues, CapabilityIssue{Kind: CapabilityKindSorts, Message: err.Error(), Scope: scope})
			return
		}
		values = loaded
	}
	identifiers := make([]string, len(values))
	for index, value := range values {
		identifiers[index] = value.ID
	}
	// FLT-55: a source that repeats an identifier within itself is discarded whole.
	if repeated, found := firstRepeated(identifiers); found {
		result.Issues = append(result.Issues, CapabilityIssue{Kind: CapabilityKindSorts, Message: "Source repeats sort " + repeated + ".", Scope: scope})
		return
	}
	if len(target)+len(values) > maximumSorts {
		result.Issues = append(result.Issues, CapabilityIssue{Kind: CapabilityKindSorts, Message: "Effective sorts exceed their limit.", Scope: scope})
		return
	}
	conflicts := map[string]bool{}
	for _, value := range values {
		if _, found := target[value.ID]; found {
			conflicts[value.ID] = true
		}
	}
	for id := range conflicts {
		delete(target, id)
		delete(scopes, id)
	}
	for _, value := range values {
		if !conflicts[value.ID] {
			target[value.ID] = value
			scopes[value.ID] = scope
		}
	}
	if len(conflicts) != 0 {
		result.Issues = append(result.Issues, CapabilityIssue{Kind: CapabilityKindSorts, Message: "Duplicate sorts: " + duplicateNames(conflicts), Scope: scope})
	}
}

func firstRepeated(identifiers []string) (string, bool) {
	seen := make(map[string]bool, len(identifiers))
	for _, id := range identifiers {
		if seen[id] {
			return id, true
		}
		seen[id] = true
	}
	return "", false
}

func sortedKeys[Value any](values map[string]Value) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func duplicateNames(duplicates map[string]bool) string {
	ids := make([]string, 0, len(duplicates))
	for id := range duplicates {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return strings.Join(ids, ", ")
}

func (client *ServiceClient) loadFilterPages(ctx context.Context, reference string, budget int) ([]odp.FilterDefinition, error) {
	values := []odp.FilterDefinition{}
	err := client.loadCapabilityPages(ctx, reference, func(data []byte) (string, error) {
		filtered, err := odp.NormalizeAgentResponse(data, "filter-page")
		if err != nil {
			return "", err
		}
		page, err := odp.ParseFilterDefinitionPage(filtered)
		if err != nil {
			return "", err
		}
		values = append(values, page.Items...)
		// FLT-58: once a source cannot fit the effective bound, stop retrieving it.
		if len(values) > budget {
			return "", errors.New("ODP linked filter source exceeds the effective filter limit")
		}
		return page.Next, nil
	})
	return values, err
}

func (client *ServiceClient) loadSortPages(ctx context.Context, reference string, budget int) ([]odp.SortDefinition, error) {
	values := []odp.SortDefinition{}
	err := client.loadCapabilityPages(ctx, reference, func(data []byte) (string, error) {
		filtered, err := odp.NormalizeAgentResponse(data, "sort-page")
		if err != nil {
			return "", err
		}
		page, err := odp.ParseSortDefinitionPage(filtered)
		if err != nil {
			return "", err
		}
		values = append(values, page.Items...)
		// FLT-58: once a source cannot fit the effective bound, stop retrieving it.
		if len(values) > budget {
			return "", errors.New("ODP linked sort source exceeds the effective sort limit")
		}
		return page.Next, nil
	})
	return values, err
}

func (client *ServiceClient) loadCapabilityPages(ctx context.Context, reference string, parse func([]byte) (string, error)) error {
	validate := func(data []byte) error {
		if jsonvalue.Depth(data) > maximumResourceDepth {
			return fmt.Errorf("%w: ODP response exceeds its nesting-depth limit", ErrResponseLimitExceeded)
		}
		return nil
	}
	visited := map[string]bool{}
	next := reference
	for page := 0; next != "" && page < maximumCapabilityPages; page++ {
		// A capability source is a Resource Reference on the Service origin, and the request that
		// retrieves it carries this client's credentials, so it must not be walked off-origin.
		target, err := odp.ResolveContinuation(next, client.serviceOrigin)
		if err != nil {
			return err
		}
		address := target.String()
		if visited[address] {
			return errors.New("ODP capability pagination loop detected")
		}
		visited[address] = true
		result, err := request(ctx, client.client, http.MethodGet, address, nil, client.acceptLanguage, client.maxRedirects, maximumResourceBytes, client.requestCache, cacheKey(client.partition, http.MethodGet, address, client.acceptLanguage, nil), client.fallbacks.CapabilityDefinition, validate)
		if err != nil {
			return err
		}
		next, err = parse(result.body)
		if err != nil {
			return err
		}
	}
	if next != "" {
		return fmt.Errorf("ODP capability source exceeded %d pages", maximumCapabilityPages)
	}
	return nil
}
