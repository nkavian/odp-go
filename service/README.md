# ODP Service package

Package `service` integrates ODP with Go's standard `net/http` stack. `Service` implements
`http.Handler` and owns the well-known document, fixed operation routes, representation defaults,
request validation, bounded JSON parsing, media negotiation, language negotiation, conditional
retrieval, response validation, and Problem Details.

## Minimum integration

Every Service supplies a valid Service Document plus `ListOfferings` and `GetOffering` catalog
functions. Those two required functions are advertised automatically. Collection and search
operations are advertised only when their corresponding functions are configured, so there is no
separate capability list to maintain.

The hosting application mounts the same handler at the well-known path and the configured endpoint
base:

```go
mux := http.NewServeMux()
mux.Handle("/.well-known/odp", odpService)
mux.Handle("/odp/", odpService)
```

The second path must match `Document.HTTP.EndpointBase`. Applications using another router perform
the equivalent two registrations.

## Small catalogs

`NewStaticCatalog` supplies the required Offering operations from an in-memory catalog. Configuring
Collections also enables Collection listing, retrieval, and direct Offering membership. It validates
identifiers and relationships at construction and uses opaque, integrity-protected stateless
continuations.

```go
catalog, err := service.NewStaticCatalog(service.StaticCatalogOptions{
	Offerings: []odp.Offering{
		{
			ID:         "gpu-h100",
			Name:       "H100 GPU",
			ODPVersion: odp.Version,
			Price:      &odp.PricePreview{Type: odp.PriceQuote},
		},
	},
})
if err != nil {
	return err
}

odpService, err := service.New(service.Options{
	Catalog: catalog,
	Document: odp.ServiceDocument{
		Branding: &odp.ServiceBranding{
			Icon: odp.ServiceBrandingImage{Source: "/branding/icon.svg", Type: odp.ServiceBrandingSVG},
			Logo: odp.ServiceBrandingImage{Source: "/branding/logo.svg", Type: odp.ServiceBrandingSVG},
		},
		Description:   "On-demand compute resources",
		HTTP: odp.HTTPConfiguration{
			EndpointBase: "/odp",
			OpenAPI:      &odp.ServiceOpenAPI{URL: "/openapi.json"},
		},
		Language:      "en",
		Localizations: []string{"en"},
		Name:          "Example Compute",
		Protocols: &odp.ServiceProtocols{
			Enrollment: []odp.EnrollmentProtocol{{Name: odp.ProtocolAEP}},
		},
	},
	OperationAuthentication: map[odp.Operation]odp.AuthenticationRequirement{
		odp.OperationGetOffering: odp.AuthenticationOptional,
	},
})
```

The static catalog defaults to 50 items per page, accepts limits through 100, and issues
continuations that expire between one and two hours after they are minted. The expiry is quantised
rather than exact so that two identical requests produce identical pages, and therefore identical
entity tags that an Agent can revalidate. Set `StaticCatalogOptions.ContinuationKey` (at least 32
bytes) so that a cursor issued by one process stays usable by another and survives a restart;
without it a key is generated at construction and outstanding cursors end with the process. Use the
static catalog for small catalogs, examples, and tests.

Branding is optional. When present, it contains both a square icon and a wide logo as SVG, PNG, or
WebP resources. Raster icons are square and at least 200 by 200 pixels; raster logos use a 4:1
aspect ratio and are at least 400 by 100 pixels. SVG resources use the corresponding aspect ratio.
Each image's optional `Type` provides a pre-retrieval format hint; set it when the resource URL does
not have a recognizable filename extension. The optional Service-wide OpenAPI document is inherited
by Offering Actions that identify only an operation ID.

Mount `odpService` at `/.well-known/odp` and its configured endpoint base. Operations default to
`not-required`; `OperationAuthentication` advertises different access requirements. Authentication,
AEP, MPP, x402, rate limiting, and application policy compose as ordinary HTTP middleware.

## Storage-backed catalogs

Large Services configure `Catalog` with functions backed by their own storage and indexes. The
runtime invokes only the function required for the incoming operation; it does not materialize,
sort, or inspect the complete catalog. Each function receives the request context, original HTTP
request, normalized representation, preferred language, limit, and opaque cursor.

Optional operation functions are the source of truth for Service Document advertisement. Search
functions receive a validated request on the initial `POST` and `nil` on a continuation `GET`, so a
Service can use either server-managed or integrity-protected stateless continuation state.

Catalog functions can return `*service.Error` for an intentional ODP Problem Details response.
Unexpected errors become a generic `500` response without exposing application details; set
`Options.OnError` to observe them, since otherwise an unexpected failure is indistinguishable from
a healthy Service to its operator.

A returned `*service.Error` is still bounded before it is sent: a status outside 400-599 or a code
outside the problem-code grammar is replaced by a generic internal error, a title is stripped of
control characters and cut to 128 code points, and a `429` or `503` gains a `Retry-After` when the
caller did not set one.

## Responses

Every response carries `ETag`, `Content-Language` and `Vary: Accept, Accept-Language`. An operation
that can authenticate — and any document that carries `auth_expands` whatever its operation declares
— adds `Authorization` to `Vary` and `Cache-Control: private`, so a shared cache cannot reuse one
authentication context's representation for another. Problem Details carry `Cache-Control: no-store`,
since a `404`, `405` or `410` is cacheable by default and outlives the condition that produced it.
`If-None-Match` is honoured: a match answers `304` on `GET` and `HEAD`, and `412` on anything else.
`HEAD` is routed and method-checked as the `GET` it is, and `net/http` takes the content back off
while keeping the `Content-Length` that `GET` would have sent.

`Accept` and `Accept-Language` are read up to a bounded number of entries, so a long header cannot
buy work proportional to its length before the request is routed.

`Accept-Language` is resolved by RFC 4647 Lookup over the Service Document's `localizations`, and
the selected tag — not the raw header — is what `CatalogRequest.Language` carries and what
`Content-Language` reports. A resource that declares its own `language` is served as that language.
When no range matches, the request stays on the default representation rather than being refused.

The runtime validates what a catalog hands back as well as what a caller sends. A page carries at
most 100 items and always serializes `items` as an array; nested items do not restate `odp_version`;
and a `next` must be at most 2048 printable ASCII characters that resolve to this Service's origin
and to something other than the request being answered. The authority is what decides that origin,
compared case-insensitively and with a default port written or left out alike; an `https` reference
is accepted even when the request arrived as `http`, because a Service behind a TLS-terminating
proxy reads `http` off the wire and publishes `https`. An Offering search may only return
refinements the request asked for, once each, and never on a continuation. A catalog that breaks one
of these answers `500` rather than emitting a document a conformant Agent would reject.

Identifiers in a path are read verbatim, with no percent-decoding, so one resource is reachable at
exactly one URL.

## Concurrency and lifecycle

`Service` and the static catalog are immutable after construction and can serve concurrent HTTP
requests. Storage-backed catalog functions remain responsible for the concurrency and cancellation
behavior of their database or remote-system operations. Each function receives the request
`context.Context`; stop work when it is canceled.

The package owns no background workers and requires no separate shutdown call. The hosting HTTP
server owns listener shutdown, connection draining, and operational telemetry.

Service Document construction uses strict current-version validation. Unknown enrollment, payment,
and trust protocol names are rejected.

## Related documentation

- [Protocol models and validation](../README.md#protocol-core)
- [Agent integration](../agent/README.md)
- [Small Service example](../examples/odp-service-small/README.md)
- [Normative specification and schemas](https://www.offeringprotocol.org/)
