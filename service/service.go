// Package service provides framework-neutral HTTP integration for ODP Services.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	odp "github.com/offering-protocol/odp-go"
	"github.com/offering-protocol/odp-go/internal/jsonvalue"
)

const (
	MediaType               = "application/odp+json"
	ProblemMediaType        = "application/problem+json"
	MaximumRequestBodyBytes = 65_536
	MaximumResourceBytes    = 524_288
)

const (
	// SVC-83: the Service Document caps at 65,536 bytes and a JSON nesting depth of 8.
	maximumDocumentBytes = 65_536
	maximumDocumentDepth = 8
	// PAG-06: next is a Resource Reference of at most 2048 ASCII characters.
	maximumNextBytes = 2_048
	// ERR-21: Problem Details cap at 16,384 bytes; every other document nests at most 16 deep.
	maximumProblemBytes  = 16_384
	maximumResourceDepth = 16
	// ERR-04: a Problem Details title carries at most 128 Unicode code points.
	maximumTitleRunes = 128
	maximumPageItems  = 100
	// A negotiation header arrives before a request is routed or authenticated, so the work it can
	// buy is bounded rather than left proportional to whatever the caller sent.
	maximumHeaderEntries = 64
)

const internalMessage = "The ODP Service could not complete the request"

// ERR-06: a problem code is 1-64 uppercase ASCII letters, digits or underscores, letter-first.
var problemCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// RFC 9110 §12.4.2: a quality value is 0 or 1 with at most three fractional digits.
var qualityValue = regexp.MustCompile(`^(?:0(?:\.[0-9]{0,3})?|1(?:\.0{0,3})?)$`)

// PAG-13: limit is an integer from 1 through 100, not merely something Atoi accepts.
var limitValue = regexp.MustCompile(`^[0-9]{1,3}$`)

type CatalogRequest struct {
	Cursor string
	// Language is the tag RFC 4647 Lookup selected for this response from the Service Document's
	// localizations (SVC-58), not the raw Accept-Language header. It is empty when the request
	// expressed no preference this Service can serve, in which case the default applies (SVC-59).
	// localizations describes the Service Document, so a catalog whose resources are localized
	// separately reads Accept-Language from Request and runs its own Lookup.
	Language       string
	Limit          int
	Representation odp.Representation
	Request        *http.Request
}

type Catalog struct {
	GetCollection           func(context.Context, string, CatalogRequest) (*odp.Collection, error)
	GetOffering             func(context.Context, string, CatalogRequest) (*odp.Offering, error)
	ListCollectionOfferings func(context.Context, string, CatalogRequest) (odp.Page[odp.Offering], error)
	ListCollections         func(context.Context, CatalogRequest) (odp.Page[odp.Collection], error)
	ListOfferings           func(context.Context, CatalogRequest) (odp.Page[odp.Offering], error)
	SearchCollections       func(context.Context, *odp.CollectionSearchRequest, CatalogRequest) (odp.Page[odp.Collection], error)
	SearchOfferings         func(context.Context, *odp.OfferingSearchRequest, CatalogRequest) (odp.OfferingPage[odp.Offering], error)
}

type Options struct {
	Catalog  Catalog
	Document odp.ServiceDocument
	// OnError observes anything a catalog handler fails with that is not an *Error, before the
	// request becomes a generic 500. Without it an unexpected failure is indistinguishable from a
	// healthy Service to its operator.
	OnError                 func(error, *http.Request)
	OperationAuthentication map[odp.Operation]odp.AuthenticationRequirement
}

type Error struct {
	Code    string
	Header  http.Header
	Message string
	Status  int
}

func (err *Error) Error() string {
	return err.Message
}

type Service struct {
	authentication map[odp.Operation]odp.AuthenticationRequirement
	catalog        Catalog
	document       odp.ServiceDocument
	endpointBase   string
	onError        func(error, *http.Request)
}

func New(options Options) (*Service, error) {
	if options.Catalog.ListOfferings == nil || options.Catalog.GetOffering == nil {
		return nil, errors.New("ODP catalog requires ListOfferings and GetOffering handlers")
	}
	operations := []odp.Operation{odp.OperationGetOffering, odp.OperationListOfferings}
	optional := []struct {
		operation odp.Operation
		enabled   bool
	}{
		{odp.OperationGetCollection, options.Catalog.GetCollection != nil},
		{odp.OperationListCollectionOfferings, options.Catalog.ListCollectionOfferings != nil},
		{odp.OperationListCollections, options.Catalog.ListCollections != nil},
		{odp.OperationSearchCollections, options.Catalog.SearchCollections != nil},
		{odp.OperationSearchOfferings, options.Catalog.SearchOfferings != nil},
	}
	for _, capability := range optional {
		if capability.enabled {
			operations = append(operations, capability.operation)
		}
	}
	sort.Slice(operations, func(left, right int) bool { return operations[left] < operations[right] })
	for operation := range options.OperationAuthentication {
		if !slices.Contains(operations, operation) {
			return nil, fmt.Errorf("authentication configured for unadvertised ODP operation %s", operation)
		}
	}
	authentication := make(map[odp.Operation]odp.AuthenticationRequirement, len(operations))
	descriptors := make([]odp.OperationDescriptor, len(operations))
	for index, operation := range operations {
		requirement := options.OperationAuthentication[operation]
		if requirement == "" {
			requirement = odp.AuthenticationNotRequired
		}
		authentication[operation] = requirement
		descriptors[index] = odp.OperationDescriptor{Authentication: requirement, Name: operation}
	}
	document := options.Document
	document.ODPVersion = odp.Version
	document.Operations = descriptors
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode ODP Service Document: %w", err)
	}
	document, err = odp.ParseServiceDocument(encoded)
	if err != nil {
		return nil, err
	}
	// SVC-84: a Service must produce a document within every limit. Core validates the shape but
	// counts neither bytes nor depth, so both budgets are enforced here, at construction.
	if len(encoded) > maximumDocumentBytes || jsonvalue.Depth(encoded) > maximumDocumentDepth {
		return nil, errors.New("ODP Service Document exceeds its resource limits")
	}
	return &Service{
		authentication: authentication,
		catalog:        options.Catalog,
		document:       document,
		endpointBase:   strings.TrimSuffix(document.HTTP.EndpointBase, "/"),
		onError:        options.OnError,
	}, nil
}

func (service *Service) Document() odp.ServiceDocument {
	encoded, _ := json.Marshal(service.document)
	var document odp.ServiceDocument
	_ = json.Unmarshal(encoded, &document)
	return document
}

func (service *Service) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	// RFC 9110: HEAD is GET without content. net/http drops the body of a HEAD response while
	// keeping the Content-Length its GET would have carried, so each route answers HEAD as its GET
	// and the transport takes the content back off.
	err := service.serve(writer, request)
	if err == nil {
		return
	}
	var serviceError *Error
	if errors.As(err, &serviceError) {
		writeProblem(writer, serviceError)
		return
	}
	service.report(err, request)
	writeProblem(writer, requestError(http.StatusInternalServerError, "INTERNAL_ERROR", internalMessage))
}

// report hands a failure to the operator's observer. An observer is the one thing in the failure
// path that must not become the failure, so a panic inside it is absorbed here.
func (service *Service) report(err error, request *http.Request) {
	if service.onError == nil {
		return
	}
	defer func() { _ = recover() }()
	service.onError(err, request)
}

func (service *Service) serve(writer http.ResponseWriter, request *http.Request) error {
	if !acceptsODP(request.Header.Get("Accept")) {
		return requestError(http.StatusNotAcceptable, "NOT_ACCEPTABLE", "Accept must allow "+MediaType)
	}
	// Every ODP operation is safe, so HEAD is routed and method-checked as the GET it is.
	method := request.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	// SVC-66: an identifier is substituted into a path template verbatim, with no percent-encoding
	// or decoding. Routing therefore reads the path as it arrived; net/http has already decoded
	// URL.Path, which would serve one resource from every encoding of its identifier.
	path := request.URL.EscapedPath()
	if path == "/.well-known/odp" {
		if err := requireMethod(method, false); err != nil {
			return err
		}
		// SVC-02: the well-known document is retrievable without enrollment or authentication.
		return respond(writer, service.document, served{
			authentication: odp.AuthenticationNotRequired, language: service.document.Language,
			maximumBytes: maximumDocumentBytes, maximumDepth: maximumDocumentDepth,
			method: method, request: request,
		})
	}
	if !strings.HasPrefix(path, service.endpointBase+"/") {
		return notFound()
	}
	switch operationPath := strings.TrimPrefix(path, service.endpointBase); operationPath {
	case "/offerings":
		return service.listOfferings(writer, request, method)
	case "/offerings/search":
		return service.searchOfferings(writer, request, method)
	case "/collections":
		return service.listCollections(writer, request, method)
	case "/collections/search":
		return service.searchCollections(writer, request, method)
	default:
		if id, ok, err := resourceID(operationPath, "/offerings/"); err != nil {
			return err
		} else if ok {
			return service.getOffering(writer, request, method, id)
		}
		if id, ok, err := collectionOfferingID(operationPath); err != nil {
			return err
		} else if ok {
			return service.listCollectionOfferings(writer, request, method, id)
		}
		if id, ok, err := resourceID(operationPath, "/collections/"); err != nil {
			return err
		} else if ok {
			return service.getCollection(writer, request, method, id)
		}
		return notFound()
	}
}

func (service *Service) listOfferings(writer http.ResponseWriter, request *http.Request, method string) error {
	if err := requireMethod(method, false); err != nil {
		return err
	}
	input, err := service.catalogRequest(request, method, odp.RepresentationTerse)
	if err != nil {
		return err
	}
	page, err := service.catalog.ListOfferings(request.Context(), input)
	if err != nil {
		return err
	}
	validated, err := offeringPage(page, input)
	if err != nil {
		return err
	}
	return service.respondPage(writer, validated, validated.AuthExpands, input, method, odp.OperationListOfferings)
}

func (service *Service) listCollections(writer http.ResponseWriter, request *http.Request, method string) error {
	// A route the Service does not advertise is absent, so it is reported that way rather than as
	// a method the resource will not take.
	if service.catalog.ListCollections == nil {
		return unsupported(odp.OperationListCollections)
	}
	if err := requireMethod(method, false); err != nil {
		return err
	}
	input, err := service.catalogRequest(request, method, odp.RepresentationTerse)
	if err != nil {
		return err
	}
	page, err := service.catalog.ListCollections(request.Context(), input)
	if err != nil {
		return err
	}
	validated, err := collectionPage(page, input)
	if err != nil {
		return err
	}
	return service.respondPage(writer, validated, validated.AuthExpands, input, method, odp.OperationListCollections)
}

func (service *Service) listCollectionOfferings(writer http.ResponseWriter, request *http.Request, method, id string) error {
	if service.catalog.ListCollectionOfferings == nil {
		return unsupported(odp.OperationListCollectionOfferings)
	}
	if err := requireMethod(method, false); err != nil {
		return err
	}
	input, err := service.catalogRequest(request, method, odp.RepresentationTerse)
	if err != nil {
		return err
	}
	page, err := service.catalog.ListCollectionOfferings(request.Context(), id, input)
	if err != nil {
		return err
	}
	validated, err := offeringPage(page, input)
	if err != nil {
		return err
	}
	return service.respondPage(writer, validated, validated.AuthExpands, input, method, odp.OperationListCollectionOfferings)
}

func (service *Service) getOffering(writer http.ResponseWriter, request *http.Request, method, id string) error {
	if err := requireMethod(method, false); err != nil {
		return err
	}
	input, err := service.catalogRequest(request, method, odp.RepresentationFull)
	if err != nil {
		return err
	}
	offering, err := service.catalog.GetOffering(request.Context(), id, input)
	if err != nil {
		return err
	}
	if offering == nil {
		return requestError(http.StatusNotFound, "NOT_FOUND", "Offering not found")
	}
	validated, err := validateOffering(*offering, input.Representation)
	if err != nil {
		return err
	}
	if validated.ID != id {
		return errors.New("Offering identifier does not match its request path")
	}
	// A single resource is a Top-Level Document, so it carries the version its page items must not.
	validated.ODPVersion = odp.Version
	return service.respondResource(writer, validated, validated.AuthExpands, validated.Language, input, method, odp.OperationGetOffering)
}

func (service *Service) getCollection(writer http.ResponseWriter, request *http.Request, method, id string) error {
	if service.catalog.GetCollection == nil {
		return unsupported(odp.OperationGetCollection)
	}
	if err := requireMethod(method, false); err != nil {
		return err
	}
	input, err := service.catalogRequest(request, method, odp.RepresentationFull)
	if err != nil {
		return err
	}
	collection, err := service.catalog.GetCollection(request.Context(), id, input)
	if err != nil {
		return err
	}
	if collection == nil {
		return requestError(http.StatusNotFound, "NOT_FOUND", "Collection not found")
	}
	validated, err := validateCollection(*collection, input.Representation)
	if err != nil {
		return err
	}
	if validated.ID != id {
		return errors.New("Collection identifier does not match its request path")
	}
	validated.ODPVersion = odp.Version
	return service.respondResource(writer, validated, validated.AuthExpands, validated.Language, input, method, odp.OperationGetCollection)
}

func (service *Service) searchOfferings(writer http.ResponseWriter, request *http.Request, method string) error {
	if service.catalog.SearchOfferings == nil {
		return unsupported(odp.OperationSearchOfferings)
	}
	if err := requireMethod(method, true); err != nil {
		return err
	}
	input, err := service.catalogRequest(request, method, odp.RepresentationTerse)
	if err != nil {
		return err
	}
	var query *odp.OfferingSearchRequest
	var refinements []string
	if method == http.MethodGet {
		if input.Cursor == "" {
			return requestError(http.StatusBadRequest, "INVALID_REQUEST", "Search continuation requires a cursor")
		}
	} else {
		body, err := readRequestBody(request)
		if err != nil {
			return err
		}
		parsed, err := odp.ParseOfferingSearchRequest(body)
		if err != nil {
			return invalidRequest(err)
		}
		query = &parsed
		refinements = parsed.Refinements
		if parsed.Limit != 0 {
			input.Limit = parsed.Limit
		}
	}
	page, err := service.catalog.SearchOfferings(request.Context(), query, input)
	if err != nil {
		return err
	}
	validated, err := offeringSearchPage(page, input, refinements)
	if err != nil {
		return err
	}
	return service.respondPage(writer, validated, validated.AuthExpands, input, method, odp.OperationSearchOfferings)
}

func (service *Service) searchCollections(writer http.ResponseWriter, request *http.Request, method string) error {
	if service.catalog.SearchCollections == nil {
		return unsupported(odp.OperationSearchCollections)
	}
	if err := requireMethod(method, true); err != nil {
		return err
	}
	input, err := service.catalogRequest(request, method, odp.RepresentationTerse)
	if err != nil {
		return err
	}
	var query *odp.CollectionSearchRequest
	if method == http.MethodGet {
		if input.Cursor == "" {
			return requestError(http.StatusBadRequest, "INVALID_REQUEST", "Search continuation requires a cursor")
		}
	} else {
		body, err := readRequestBody(request)
		if err != nil {
			return err
		}
		parsed, err := odp.ParseCollectionSearchRequest(body)
		if err != nil {
			return invalidRequest(err)
		}
		query = &parsed
		if parsed.Limit != 0 {
			input.Limit = parsed.Limit
		}
	}
	page, err := service.catalog.SearchCollections(request.Context(), query, input)
	if err != nil {
		return err
	}
	validated, err := collectionPage(page, input)
	if err != nil {
		return err
	}
	return service.respondPage(writer, validated, validated.AuthExpands, input, method, odp.OperationSearchCollections)
}

func (service *Service) respondPage(writer http.ResponseWriter, page any, expands bool, input CatalogRequest, method string, operation odp.Operation) error {
	return respond(writer, page, served{
		authentication: service.authentication[operation], authExpands: expands,
		language:     representationLanguage("", input.Language, service.document.Language),
		maximumBytes: MaximumResourceBytes, maximumDepth: maximumResourceDepth,
		method: method, request: input.Request,
	})
}

func (service *Service) respondResource(writer http.ResponseWriter, resource any, expands bool, language string, input CatalogRequest, method string, operation odp.Operation) error {
	return respond(writer, resource, served{
		authentication: service.authentication[operation], authExpands: expands,
		language:     representationLanguage(language, input.Language, service.document.Language),
		maximumBytes: MaximumResourceBytes, maximumDepth: maximumResourceDepth,
		method: method, request: input.Request,
	})
}

// served is what one answered request needs beyond its body: how it was served, and the policy
// that decides how it may be cached.
type served struct {
	authentication odp.AuthenticationRequirement
	authExpands    bool
	language       string
	maximumBytes   int
	maximumDepth   int
	method         string
	request        *http.Request
}

func respond(writer http.ResponseWriter, value any, exchange served) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	// ERR-19: a conformant Agent stops reading an over-limit document (ERR-20), so emitting one
	// produces a response the caller cannot use. It fails here instead.
	if len(encoded) > exchange.maximumBytes || jsonvalue.Depth(encoded) > exchange.maximumDepth {
		return errors.New("ODP response exceeds its resource limits")
	}
	tag := entityTag(exchange.language, encoded)
	matched := matchesEntityTag(exchange.request.Header.Get("If-None-Match"), tag)
	// RFC 9110 §13.1.2: a matched If-None-Match is 304 for GET and HEAD, 412 for anything else.
	if matched && exchange.method != http.MethodGet {
		return requestError(http.StatusPreconditionFailed, "PRECONDITION_FAILED", "If-None-Match matched the current representation")
	}
	// A representation is caller-specific when the operation lets the caller authenticate, and also
	// when the document itself says authenticating expands it (REP-08, PAG-04) whatever the
	// operation declares.
	authenticated := exchange.authExpands ||
		exchange.authentication == odp.AuthenticationOptional ||
		exchange.authentication == odp.AuthenticationRequired
	vary := "Accept, Accept-Language"
	if authenticated {
		// A representation an operation can authenticate must not be reused for a different
		// authentication context, which a shared cache can only honour when the response says so.
		vary += ", Authorization"
		writer.Header().Set("Cache-Control", "private")
	}
	writer.Header().Set("Content-Language", exchange.language)
	writer.Header().Set("Content-Type", MediaType)
	writer.Header().Set("ETag", tag)
	writer.Header().Set("Vary", vary)
	// PAG-31: honour conditional retrieval so an Agent's revalidation is not a full transfer.
	if matched {
		writer.WriteHeader(http.StatusNotModified)
		return nil
	}
	// Once the first byte is on the wire the response is committed, so a failed write is the
	// connection ending rather than a failure this Service could answer with a different status.
	_, _ = writer.Write(encoded)
	return nil
}

// entityTag is a strong validator over the negotiated language and the exact bytes served. Hashing
// the body alone would give two language variants of an unlocalized body one validator (SVC-61).
func entityTag(language string, body []byte) string {
	digest := sha256.New()
	digest.Write([]byte(language))
	digest.Write([]byte{' '})
	digest.Write(body)
	return `"` + base64.RawURLEncoding.EncodeToString(digest.Sum(nil))[:27] + `"`
}

func matchesEntityTag(header, tag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		// RFC 9110 compares If-None-Match validators weakly, so W/"x" matches the strong "x" this
		// Service issues.
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" || candidate == tag {
			return true
		}
	}
	return false
}

func (service *Service) catalogRequest(request *http.Request, method string, defaultRepresentation odp.Representation) (CatalogRequest, error) {
	query := request.URL.Query()
	representationValues := query["representation"]
	if len(representationValues) > 1 {
		return CatalogRequest{}, requestError(http.StatusBadRequest, "INVALID_REQUEST", "representation must not be repeated")
	}
	representation := defaultRepresentation
	if len(representationValues) == 1 {
		representation = odp.Representation(representationValues[0])
	}
	if representation != odp.RepresentationTerse && representation != odp.RepresentationFull {
		return CatalogRequest{}, requestError(http.StatusBadRequest, "INVALID_REQUEST", "representation must be terse or full")
	}
	limit, err := queryLimit(query, method)
	if err != nil {
		return CatalogRequest{}, err
	}
	cursorValues := query["cursor"]
	if len(cursorValues) > 1 {
		return CatalogRequest{}, requestError(http.StatusBadRequest, "INVALID_REQUEST", "cursor must not be repeated")
	}
	cursor := ""
	if len(cursorValues) == 1 {
		cursor = cursorValues[0]
	}
	return CatalogRequest{
		Cursor:         cursor,
		Language:       selectLanguage(request.Header.Get("Accept-Language"), service.document.Language, service.document.Localizations),
		Limit:          limit,
		Representation: representation,
		Request:        request,
	}, nil
}

// queryLimit reads PAG-13's limit: a POST search carries it as a request-body member, a GET in the
// query. Accepting both would let one request name two page sizes.
func queryLimit(query url.Values, method string) (int, error) {
	values := query["limit"]
	if method == http.MethodPost {
		if len(values) != 0 {
			return 0, requestError(http.StatusBadRequest, "INVALID_REQUEST", "A POST search carries limit as a request-body member")
		}
		return 0, nil
	}
	if len(values) > 1 {
		return 0, requestError(http.StatusBadRequest, "INVALID_REQUEST", "limit must not be repeated")
	}
	if len(values) == 0 {
		return 0, nil
	}
	if !limitValue.MatchString(values[0]) {
		return 0, requestError(http.StatusBadRequest, "INVALID_REQUEST", "limit must be an integer from 1 through 100")
	}
	limit, err := strconv.Atoi(values[0])
	if err != nil || limit < 1 || limit > maximumPageItems {
		return 0, requestError(http.StatusBadRequest, "INVALID_REQUEST", "limit must be an integer from 1 through 100")
	}
	return limit, nil
}

func readRequestBody(request *http.Request) ([]byte, error) {
	contentType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(contentType, MediaType) {
		return nil, requestError(http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be "+MediaType)
	}
	if request.ContentLength > MaximumRequestBodyBytes {
		return nil, requestError(http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "ODP request body exceeds its byte limit")
	}
	data, err := io.ReadAll(io.LimitReader(request.Body, MaximumRequestBodyBytes+1))
	if err != nil {
		return nil, requestError(http.StatusBadRequest, "INVALID_REQUEST", "ODP request body is invalid")
	}
	if len(data) > MaximumRequestBodyBytes {
		return nil, requestError(http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "ODP request body exceeds its byte limit")
	}
	if !utf8.Valid(data) {
		return nil, requestError(http.StatusBadRequest, "INVALID_REQUEST", "ODP request body must use UTF-8")
	}
	if depth := jsonvalue.Depth(data); depth > maximumResourceDepth {
		return nil, requestError(http.StatusBadRequest, "INVALID_REQUEST", "ODP request body exceeds its JSON depth limit")
	}
	return data, nil
}

func offeringPage(page odp.Page[odp.Offering], input CatalogRequest) (odp.Page[odp.Offering], error) {
	items, err := validateItems(page.Items, input.Representation, validateOffering)
	if err != nil {
		return odp.Page[odp.Offering]{}, err
	}
	page.Items = items
	page.ODPVersion = odp.Version
	if err := requireContinuation(page.Next, input.Request); err != nil {
		return odp.Page[odp.Offering]{}, err
	}
	// A catalog supplies next and auth_expands alongside its items, so the envelope is validated
	// as a whole rather than one item at a time.
	encoded, err := json.Marshal(page)
	if err != nil {
		return odp.Page[odp.Offering]{}, err
	}
	return odp.ParsePage[odp.Offering](encoded)
}

func collectionPage(page odp.Page[odp.Collection], input CatalogRequest) (odp.Page[odp.Collection], error) {
	items, err := validateItems(page.Items, input.Representation, validateCollection)
	if err != nil {
		return odp.Page[odp.Collection]{}, err
	}
	page.Items = items
	page.ODPVersion = odp.Version
	if err := requireContinuation(page.Next, input.Request); err != nil {
		return odp.Page[odp.Collection]{}, err
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		return odp.Page[odp.Collection]{}, err
	}
	return odp.ParsePage[odp.Collection](encoded)
}

func offeringSearchPage(page odp.OfferingPage[odp.Offering], input CatalogRequest, requested []string) (odp.OfferingPage[odp.Offering], error) {
	items, err := validateItems(page.Items, input.Representation, validateOffering)
	if err != nil {
		return odp.OfferingPage[odp.Offering]{}, err
	}
	page.Items = items
	page.ODPVersion = odp.Version
	if err := requireContinuation(page.Next, input.Request); err != nil {
		return odp.OfferingPage[odp.Offering]{}, err
	}
	if err := requireRefinements(page.Refinements, input, requested); err != nil {
		return odp.OfferingPage[odp.Offering]{}, err
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		return odp.OfferingPage[odp.Offering]{}, err
	}
	return odp.ParseOfferingSearchResponse(encoded)
}

func validateItems[Item any](items []Item, representation odp.Representation, validate func(Item, odp.Representation) (Item, error)) ([]Item, error) {
	if len(items) > maximumPageItems {
		return nil, fmt.Errorf("ODP page contains more than %d items", maximumPageItems)
	}
	// items is a required array member, so a page that yielded nothing still says so.
	validated := make([]Item, 0, len(items))
	for _, item := range items {
		value, err := validate(item, representation)
		if err != nil {
			return nil, err
		}
		validated = append(validated, value)
	}
	return validated, nil
}

// requireContinuation checks the envelope members the page schema does not constrain: a next that
// is over-long, non-ASCII, off-origin or unchanged would send a conformant Agent somewhere it must
// not go, or nowhere at all.
func requireContinuation(next string, request *http.Request) error {
	if next == "" {
		return nil
	}
	if len(next) > maximumNextBytes {
		return errors.New("ODP continuation reference exceeds its length limit")
	}
	for _, character := range next {
		if character < 0x20 || character > 0x7e {
			return errors.New("ODP continuation reference must be printable ASCII")
		}
	}
	reference, err := url.Parse(next)
	if err != nil {
		return errors.New("ODP continuation reference is not a valid reference")
	}
	current := currentURL(request)
	resolved := current.ResolveReference(reference)
	// PAG-07: next must resolve to the same origin as the initial operation. The authority is what
	// decides that. The scheme may be https even when the request arrived as http, because a
	// Service behind a TLS-terminating proxy reads http off the wire and publishes https.
	if resolved.User != nil || !sameAuthority(resolved, current) ||
		!(strings.EqualFold(resolved.Scheme, "https") || strings.EqualFold(resolved.Scheme, current.Scheme)) {
		return errors.New("ODP continuation reference must remain on the Service origin")
	}
	// PAG-11: a continuation must advance traversal, so it cannot point back at this request.
	if resolved.EscapedPath() == current.EscapedPath() && resolved.RawQuery == current.RawQuery {
		return errors.New("ODP continuation reference must advance past this request")
	}
	return nil
}

// sameAuthority compares two authorities the way RFC 3986 6.2.3 does: case-insensitively, and with
// a port that is the default for its scheme written and left out alike.
func sameAuthority(left, right *url.URL) bool {
	return strings.EqualFold(left.Hostname(), right.Hostname()) && statedPort(left) == statedPort(right)
}

func statedPort(value *url.URL) string {
	port := value.Port()
	if (port == "443" && strings.EqualFold(value.Scheme, "https")) ||
		(port == "80" && strings.EqualFold(value.Scheme, "http")) {
		return ""
	}
	return port
}

// currentURL is the absolute URL this request was made to. A server-side request carries its
// authority in Host rather than in the request target, so the origin is reassembled here.
func currentURL(request *http.Request) *url.URL {
	scheme := request.URL.Scheme
	if scheme == "" {
		scheme = "http"
		if request.TLS != nil {
			scheme = "https"
		}
	}
	host := request.Host
	if host == "" {
		host = request.URL.Host
	}
	return &url.URL{Host: host, Path: request.URL.Path, RawPath: request.URL.RawPath, RawQuery: request.URL.RawQuery, Scheme: scheme}
}

func requireRefinements(groups []odp.RefinementGroup, input CatalogRequest, requested []string) error {
	if len(groups) == 0 {
		return nil
	}
	// OFR-15: only the initial response of a search may carry refinements.
	if input.Cursor != "" {
		return errors.New("ODP Offering search continuation cannot contain refinements")
	}
	seen := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		// OFR-14 and FLT-30: every filter_id occurs in the request and is unique among the groups.
		if !slices.Contains(requested, group.FilterID) {
			return fmt.Errorf("ODP Offering search returned refinement %s that was not requested", group.FilterID)
		}
		if _, duplicate := seen[group.FilterID]; duplicate {
			return fmt.Errorf("ODP Offering search returned refinement %s more than once", group.FilterID)
		}
		seen[group.FilterID] = struct{}{}
	}
	return nil
}

func validateOffering(offering odp.Offering, representation odp.Representation) (odp.Offering, error) {
	forValidation := offering
	forValidation.ODPVersion = odp.Version
	encoded, err := json.Marshal(forValidation)
	if err != nil {
		return odp.Offering{}, err
	}
	validated, err := odp.ParseOffering(encoded)
	if err != nil {
		return odp.Offering{}, err
	}
	if representation == odp.RepresentationTerse && len(validated.Actions) != 0 {
		return odp.Offering{}, errors.New("ODP Terse Offering cannot contain Actions")
	}
	actionIDs := make(map[string]struct{}, len(validated.Actions))
	for _, action := range validated.Actions {
		if _, duplicate := actionIDs[action.ID]; duplicate {
			return odp.Offering{}, errors.New("ODP Offering Action identifiers must be unique")
		}
		actionIDs[action.ID] = struct{}{}
	}
	if representation == odp.RepresentationFull && len(validated.DetailFields) != 0 {
		return odp.Offering{}, errors.New("ODP Full Offering cannot contain detail_fields")
	}
	// VER-03: an item nested in a page inherits its container's version and must not restate it.
	// The routes that serve one resource put the version back.
	validated.ODPVersion = ""
	return validated, nil
}

func validateCollection(collection odp.Collection, representation odp.Representation) (odp.Collection, error) {
	forValidation := collection
	forValidation.ODPVersion = odp.Version
	encoded, err := json.Marshal(forValidation)
	if err != nil {
		return odp.Collection{}, err
	}
	validated, err := odp.ParseCollection(encoded)
	if err != nil {
		return odp.Collection{}, err
	}
	if representation == odp.RepresentationFull && len(validated.DetailFields) != 0 {
		return odp.Collection{}, errors.New("ODP Full Collection cannot contain detail_fields")
	}
	validated.ODPVersion = ""
	return validated, nil
}

func writeProblem(writer http.ResponseWriter, problem *Error) {
	status, code, title := problem.Status, problem.Code, problem.Message
	// A status outside the error range is not a failure this Service can describe, so the problem
	// is replaced whole rather than half-corrected into a code and a status that disagree.
	if status < 400 || status > 599 {
		status, code, title = http.StatusInternalServerError, "INTERNAL_ERROR", internalMessage
	}
	if !problemCode.MatchString(code) {
		code = "INTERNAL_ERROR"
	}
	details := problemDetails(code, boundedTitle(title, status), status)
	encoded, err := json.Marshal(details)
	if err == nil {
		_, err = odp.ParseProblemResponse(encoded, status)
	}
	if err != nil || len(encoded) > maximumProblemBytes || jsonvalue.Depth(encoded) > maximumResourceDepth {
		status, code = http.StatusInternalServerError, "INTERNAL_ERROR"
		encoded, _ = json.Marshal(problemDetails(code, internalMessage, status))
		// A downgraded problem is the Service's own, and must not answer with headers the caller
		// chose for a status it is no longer sending.
		problem = &Error{}
	}
	header := writer.Header()
	if status == problem.Status && code == problem.Code {
		for name, values := range problem.Header {
			for _, value := range values {
				header.Add(name, value)
			}
		}
	}
	// ERR-32: a 429 must carry Retry-After, and ERR-33 recommends it on 503. Without it an Agent's
	// retry policy has nothing to honour.
	if (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable) && header.Get("Retry-After") == "" {
		header.Set("Retry-After", "1")
	}
	// RFC 9110 15.1 makes 404, 405 and 410 cacheable by default, and a stored failure outlives the
	// condition that caused it.
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Type", ProblemMediaType)
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}

func problemDetails(code, title string, status int) odp.ProblemDetails {
	return odp.ProblemDetails{
		Code: code, Status: status, Title: title,
		Type: "https://offeringprotocol.org/problems/" + strings.ToLower(strings.ReplaceAll(code, "_", "-")),
	}
}

// boundedTitle keeps a title inside ERR-04's 128 code points and free of the control characters
// that would let it forge a log line in whatever reads it.
func boundedTitle(title string, status int) string {
	candidate := strings.TrimSpace(strings.Map(func(character rune) rune {
		// unicode.IsControl covers C0, DEL and C1. The two separators are not control characters
		// but end a line for anything that reads the title back as text.
		if unicode.IsControl(character) || character == '\u2028' || character == '\u2029' {
			return ' '
		}
		return character
	}, title))
	if candidate == "" {
		candidate = fmt.Sprintf("ODP request failed with HTTP %d", status)
	}
	if runes := []rune(candidate); len(runes) > maximumTitleRunes {
		return string(runes[:maximumTitleRunes])
	}
	return candidate
}

func acceptsODP(accept string) bool {
	if strings.TrimSpace(accept) == "" {
		return true
	}
	bestSpecificity := -1
	bestQuality := 0.0
	for entry := range headerEntries(accept) {
		specificity := -1
		switch rangeOf(entry) {
		case MediaType:
			specificity = 2
		case "application/*":
			specificity = 1
		case "*/*":
			specificity = 0
		}
		if specificity < 0 {
			continue
		}
		// RFC 9110 §12.4.2: an entry whose q is outside the quality grammar has no weight at all,
		// so a malformed parameter cannot promote it above a well-formed one.
		quality, ok := qualityOf(entry)
		if !ok {
			continue
		}
		// The most specific range that matches decides, so q=0 on the exact media type refuses the
		// request even alongside a */* the caller would otherwise accept.
		if specificity > bestSpecificity || (specificity == bestSpecificity && quality > bestQuality) {
			bestSpecificity, bestQuality = specificity, quality
		}
	}
	return bestSpecificity >= 0 && bestQuality > 0
}

// selectLanguage runs RFC 4647 Lookup over the localizations the Service advertises (SVC-58). It
// returns "" when no range matches, which leaves the request on the default representation rather
// than answering 406 (SVC-59).
func selectLanguage(header, fallback string, localizations []string) string {
	type preference struct {
		quality float64
		value   string
	}
	var entries []preference
	for entry := range headerEntries(header) {
		value := rangeOf(entry)
		quality, ok := qualityOf(entry)
		if value == "" || !ok {
			continue
		}
		entries = append(entries, preference{quality: quality, value: value})
	}
	var wanted []preference
	var refused []string
	residual := false
	for _, entry := range entries {
		switch {
		case entry.value == "*":
			residual = residual || entry.quality > 0
		case entry.quality > 0:
			wanted = append(wanted, entry)
		default:
			// RFC 9110 §12.4.2: q=0 marks a range unacceptable. It carves tags out of the residual
			// below rather than competing for a match of its own.
			refused = append(refused, entry.value)
		}
	}
	sort.SliceStable(wanted, func(left, right int) bool { return wanted[left].quality > wanted[right].quality })
	for _, entry := range wanted {
		if found := lookup(entry.value, localizations); found != "" {
			return found
		}
	}
	// RFC 9110 §12.5.4: * matches every tag no other range in the field matched, so it is the
	// residual and can never outrank a range the caller named.
	if !residual {
		return ""
	}
	for _, tag := range append([]string{fallback}, localizations...) {
		if !slices.ContainsFunc(refused, func(value string) bool { return covers(value, strings.ToLower(tag)) }) {
			return tag
		}
	}
	return ""
}

// lookup is RFC 4647 Lookup: truncate the range at its subtag boundaries until a tag matches it.
func lookup(value string, localizations []string) string {
	for candidate := value; ; {
		for _, tag := range localizations {
			if strings.EqualFold(tag, candidate) {
				return tag
			}
		}
		cut := strings.LastIndex(candidate, "-")
		if cut < 0 {
			return ""
		}
		candidate = candidate[:cut]
		// A single-character subtag is an extension or private-use singleton; drop it with its
		// parent rather than matching on it alone.
		if singleton := strings.LastIndex(candidate, "-"); singleton >= 0 && len(candidate)-singleton == 2 {
			candidate = candidate[:singleton]
		}
	}
}

// covers is RFC 4647 basic filtering: a range covers the tag it equals and any tag it prefixes.
func covers(value, tag string) bool {
	return tag == value || strings.HasPrefix(tag, value+"-")
}

// headerEntries yields the comma-separated entries of a negotiation header, up to the bound. It
// walks the header rather than splitting it, so a header of many entries costs no allocation.
func headerEntries(header string) iter.Seq[string] {
	return func(yield func(string) bool) {
		remaining := maximumHeaderEntries
		for entry := range strings.SplitSeq(header, ",") {
			if remaining == 0 || !yield(entry) {
				return
			}
			remaining--
		}
	}
}

// rangeOf is one Accept or Accept-Language entry, lowercased and without its parameters.
func rangeOf(entry string) string {
	if semicolon := strings.Index(entry, ";"); semicolon >= 0 {
		entry = entry[:semicolon]
	}
	return strings.ToLower(strings.TrimSpace(entry))
}

// qualityOf is the entry's weight. An entry with no q parameter carries the default weight of 1;
// one whose q is outside the RFC 9110 grammar has no weight at all and reports so.
func qualityOf(entry string) (float64, bool) {
	semicolon := strings.Index(entry, ";")
	if semicolon < 0 {
		return 1, true
	}
	for _, parameter := range strings.Split(entry[semicolon+1:], ";") {
		name, value, found := strings.Cut(parameter, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		value = strings.TrimSpace(value)
		if !qualityValue.MatchString(value) {
			return 0, false
		}
		quality, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, false
		}
		return quality, true
	}
	return 1, true
}

func requireMethod(method string, post bool) error {
	if method == http.MethodGet || (post && method == http.MethodPost) {
		return nil
	}
	// RFC 9110 requires Allow to enumerate every method the resource supports. Every ODP operation
	// answers GET, so HEAD is always among them.
	allow := "GET, HEAD"
	if post {
		allow = "GET, HEAD, POST"
	}
	return &Error{
		Code: "METHOD_NOT_ALLOWED", Header: http.Header{"Allow": []string{allow}},
		Message: "ODP operation requires " + allow, Status: http.StatusMethodNotAllowed,
	}
}

func resourceID(path, prefix string) (string, bool, error) {
	if !strings.HasPrefix(path, prefix) {
		return "", false, nil
	}
	suffix := strings.TrimPrefix(path, prefix)
	if suffix == "" || strings.Contains(suffix, "/") {
		return "", false, nil
	}
	return identifier(suffix)
}

func collectionOfferingID(path string) (string, bool, error) {
	const prefix = "/collections/"
	const suffix = "/offerings"
	// The prefix and the suffix must not overlap, or a Collection identified as "offerings" would
	// be read as the members of a Collection with no identifier at all.
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) || len(path) <= len(prefix)+len(suffix) {
		return "", false, nil
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if id == "" || strings.Contains(id, "/") {
		return "", false, nil
	}
	return identifier(id)
}

// identifier reads a path segment as SVC-66 writes one: verbatim, with no percent-decoding, so one
// resource is reachable at exactly one URL.
func identifier(value string) (string, bool, error) {
	if !odp.IsLocalResourceIdentifier(value) {
		return "", false, requestError(http.StatusBadRequest, "INVALID_REQUEST", "Resource identifier is malformed")
	}
	return value, true, nil
}

func requestError(status int, code, message string) *Error {
	return &Error{Code: code, Message: message, Status: status}
}

func notFound() *Error {
	return requestError(http.StatusNotFound, "NOT_FOUND", "ODP resource not found")
}

func unsupported(operation odp.Operation) *Error {
	return requestError(http.StatusNotFound, "NOT_FOUND", string(operation)+" is not supported")
}

func invalidRequest(err error) *Error {
	var validation *odp.ValidationError
	if errors.As(err, &validation) {
		return requestError(http.StatusBadRequest, "INVALID_REQUEST", validation.Error())
	}
	return requestError(http.StatusBadRequest, "INVALID_REQUEST", "ODP request body is invalid")
}

// representationLanguage is the language actually served: the resource's own tag when it declares
// one, otherwise the tag Lookup selected for this request, otherwise the Service default.
func representationLanguage(declared, selected, fallback string) string {
	if declared != "" {
		return declared
	}
	if selected != "" {
		return selected
	}
	return fallback
}
