package directory

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	odp "github.com/offering-protocol/odp-go"
)

func parseIndexedService(data []byte) (IndexedService, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return IndexedService{}, err
	}
	id, err := requiredText(object["service_id"], "service_id", 1, 128)
	if err != nil {
		return IndexedService{}, err
	}
	source, err := parseSource(object["source"])
	if err != nil {
		return IndexedService{}, err
	}
	var service Service
	if source.Type == SourceODP {
		service, err = parseService(data)
	} else {
		service, err = parseImportedService(data, object)
	}
	if err != nil {
		return IndexedService{}, err
	}
	delete(service.Additional, "service_id")
	delete(service.Additional, "source")
	return IndexedService{Service: service, ServiceID: id, Source: source}, nil
}

func parseSource(data []byte) (Source, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return Source{}, err
	}
	kind, err := requiredText(object["type"], "source.type", 1, 128)
	if err != nil {
		return Source{}, err
	}
	address, err := requiredText(object["url"], "source.url", 1, 2048)
	if err != nil {
		return Source{}, err
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || strings.Contains(address, "#") || !publicHTTPSOrigin(address) {
		return Source{}, errors.New("source.url must be a public HTTPS document URL without credentials or a fragment")
	}
	var discovery *bool
	if err := json.Unmarshal(object["x402_discovery"], &discovery); err != nil || discovery == nil {
		return Source{}, errors.New("source.x402_discovery must be a boolean")
	}
	return Source{
		Additional: cloneAdditional(object, "type", "url", "x402_discovery"),
		Type:       SourceType(kind), URL: address, X402Discovery: *discovery,
	}, nil
}

func parseImportedService(data []byte, object map[string]json.RawMessage) (Service, error) {
	reference, err := parseServiceReference(data)
	if err != nil {
		return Service{}, err
	}
	name, err := requiredText(object["name"], "name", 1, 128)
	if err != nil {
		return Service{}, err
	}
	stamp, err := requiredText(object["indexed_at"], "indexed_at", 1, 64)
	if err != nil {
		return Service{}, err
	}
	indexedAt, err := time.Parse(time.RFC3339Nano, strings.ToUpper(stamp))
	if err != nil {
		return Service{}, errors.New("indexed_at must be a date-time")
	}
	service := Service{Name: name, ServiceOrigin: reference.ServiceOrigin, IndexedAt: indexedAt}
	for field, destination := range map[string]any{
		"description": &service.Description, "documentation_url": &service.DocumentationURL,
		"keywords": &service.Keywords, "language": &service.Language, "localizations": &service.Localizations,
		"status_url": &service.StatusURL, "support_url": &service.SupportURL, "website_url": &service.WebsiteURL,
	} {
		if raw, present := object[field]; present {
			if string(raw) == "null" || json.Unmarshal(raw, destination) != nil {
				return Service{}, fmt.Errorf("%s is invalid", field)
			}
		}
	}
	if raw, present := object["protocols"]; present {
		service.Protocols, err = parseImportedProtocols(raw)
		if err != nil {
			return Service{}, err
		}
	}
	known := append([]string{
		"service_id", "source", "service_origin", "name", "description", "documentation_url", "language", "localizations",
		"keywords", "operations", "protocols", "indexed_at", "status_url", "support_url", "website_url",
	}, unverifiedMembers...)
	service.Additional = cloneAdditional(object, known...)
	return service, nil
}

func parseImportedProtocols(data []byte) (*odp.ServiceProtocols, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return nil, errors.New("protocols must be an object")
	}
	enrollment, err := importedDescriptors(object["enrollment"], []odp.Protocol{odp.ProtocolAEP}, parseEnrollment)
	if err != nil {
		return nil, err
	}
	payments, err := importedDescriptors(object["payments"], []odp.Protocol{odp.ProtocolMPP, odp.ProtocolX402}, parsePayment)
	if err != nil {
		return nil, err
	}
	trust, err := importedDescriptors(object["trust"], []odp.Protocol{odp.ProtocolTAP}, parseTrust)
	if err != nil {
		return nil, err
	}
	return &odp.ServiceProtocols{Enrollment: enrollment, Payments: payments, Trust: trust}, nil
}

func importedDescriptors[T any](data []byte, known []odp.Protocol, parse func(json.RawMessage) (T, error)) ([]T, error) {
	if data == nil {
		return nil, nil
	}
	var descriptors []json.RawMessage
	if json.Unmarshal(data, &descriptors) != nil || descriptors == nil {
		return nil, errors.New("protocol descriptors must be an array")
	}
	var result []T
	for _, raw := range descriptors {
		var descriptor struct {
			Name odp.Protocol `json:"name"`
		}
		if json.Unmarshal(raw, &descriptor) != nil || descriptor.Name == "" {
			return nil, errors.New("protocol descriptor must have a name")
		}
		if !slices.Contains(known, descriptor.Name) {
			continue
		}
		value, err := parse(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}
