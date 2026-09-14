package odp_test

import (
	"testing"

	odp "github.com/offering-protocol/odp-go"
)

func TestOriginAndReferenceBoundaries(t *testing.T) {
	for _, input := range []string{"https://%", "https://user:secret@example.com", "/relative", "http://example.com", "https://\u200d.example"} {
		if _, err := odp.DeriveServiceOrigin(input); err == nil {
			t.Fatalf("accepted origin %q", input)
		}
	}
	for _, tc := range []struct{ input, want string }{
		{"https://EXAMPLE.com:443/document", "https://example.com"},
		{"https://example.com:8443/document", "https://example.com:8443"},
		{"http://localhost:80/document", "http://localhost"},
		{"http://[::1]:9900/document", "http://[::1]:9900"},
		{"https://[2001:db8::1]:443/document", "https://[2001:db8::1]"},
		{"https://bücher.example/document", "https://xn--bcher-kva.example"},
	} {
		got, err := odp.DeriveServiceOrigin(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("%q = %q, %v", tc.input, got, err)
		}
	}
	for _, tc := range []struct{ ref, origin string }{
		{"//evil.example/a", "https://example.com"}, {"relative", "https://example.com"},
		{"/a", "https://%"}, {"/%", "https://example.com"},
		{"/a#fragment", "https://example.com"}, {"https://user:secret@example.com/a", "https://example.com"},
		{"http://localhost.evil.example/a", "https://example.com"}, {"/a", ""},
	} {
		if _, err := odp.ResolveResourceReference(tc.ref, tc.origin); err == nil {
			t.Fatalf("accepted %#v", tc)
		}
		if _, err := odp.ResolveContinuation(tc.ref, tc.origin); err == nil {
			t.Fatalf("accepted continuation %#v", tc)
		}
	}
	for _, origin := range []string{"https://example.com", "https://\u200d.example"} {
		if _, err := odp.ResolveContinuation("https://\u200d.example/a", origin); err == nil {
			t.Fatal("accepted invalid IDNA origin")
		}
	}
	if _, err := odp.ResolveContinuation("https://example.com/a", "https://\u200d.example"); err == nil {
		t.Fatal("accepted invalid base origin")
	}
	if _, err := odp.ResolveContinuation("https://other.example/a", "https://example.com"); err == nil {
		t.Fatal("accepted cross-origin continuation")
	}
	got, err := odp.ResolveContinuation("https://EXAMPLE.com:443/a?cursor=opaque", "https://example.com")
	if err != nil || got.RawQuery != "cursor=opaque" {
		t.Fatalf("same origin continuation = %v, %v", got, err)
	}
}

func TestEveryOperationURL(t *testing.T) {
	for _, tc := range []struct {
		op       odp.Operation
		id, path string
	}{
		{odp.OperationListCollections, "", "/collections"}, {odp.OperationSearchCollections, "", "/collections/search"},
		{odp.OperationGetCollection, "plants", "/collections/plants"}, {odp.OperationListCollectionOfferings, "plants", "/collections/plants/offerings"},
		{odp.OperationListOfferings, "", "/offerings"}, {odp.OperationSearchOfferings, "", "/offerings/search"},
		{odp.OperationGetOffering, "search", "/offerings/search"},
	} {
		got, err := odp.BuildOperationURL("/odp/", tc.op, "https://example.com", tc.id)
		if err != nil || got.String() != "https://example.com/odp"+tc.path {
			t.Fatalf("%s: %v, %v", tc.op, got, err)
		}
		invalidID := "unexpected"
		if tc.id != "" {
			invalidID = "../escape"
		}
		if _, err := odp.BuildOperationURL("/odp", tc.op, "https://example.com", invalidID); err == nil {
			t.Fatalf("accepted invalid id for %s", tc.op)
		}
	}
	for _, base := range []string{"odp", "//evil.example"} {
		if _, err := odp.BuildOperationURL(base, odp.OperationListOfferings, "https://example.com", ""); err == nil {
			t.Fatalf("accepted base %s", base)
		}
	}
	if _, err := odp.BuildOperationURL("/odp", "future", "https://example.com", ""); err == nil {
		t.Fatal("accepted unknown operation")
	}
}
