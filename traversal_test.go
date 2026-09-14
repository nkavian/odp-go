package odp_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	odp "github.com/offering-protocol/odp-go"
)

func TestPageIterationFailureAndEarlyStop(t *testing.T) {
	wantErr := errors.New("connection closed")
	for _, cancel := range []bool{false, true} {
		ctx, stop := context.WithCancel(context.Background())
		if cancel {
			stop()
		}
		calls := 0
		var gotErr error
		var items []int
		for item, err := range odp.IterateItems(ctx, odp.Page[int]{Items: []int{1}, Next: "/next"}, func(context.Context, string) (odp.Page[int], error) {
			calls++
			return odp.Page[int]{}, wantErr
		}) {
			if err != nil {
				gotErr = err
			} else {
				items = append(items, item)
			}
		}
		stop()
		if cancel {
			if !errors.Is(gotErr, context.Canceled) || calls != 0 {
				t.Fatalf("cancellation: %v, calls %d", gotErr, calls)
			}
		} else if !errors.Is(gotErr, wantErr) || calls != 1 {
			t.Fatalf("loader failure: %v, calls %d", gotErr, calls)
		}
		if !reflect.DeepEqual(items, []int{1}) {
			t.Fatalf("items: %v", items)
		}
	}
	for _, itemsMode := range []bool{false, true} {
		load := func(context.Context, string) (odp.Page[int], error) {
			t.Fatal("loaded after consumer stopped")
			return odp.Page[int]{}, nil
		}
		first := odp.Page[int]{Items: []int{1, 2}, Next: "/next"}
		if itemsMode {
			for range odp.IterateItems(context.Background(), first, load) {
				break
			}
		} else {
			for range odp.IteratePages(context.Background(), first, load) {
				break
			}
		}
	}
}

func TestPageIterationPreservesOrderAndStopsAtEnd(t *testing.T) {
	calls := 0
	var got []int
	for item, err := range odp.IterateItems(context.Background(), odp.Page[int]{Items: []int{1, 2}, Next: "/next"}, func(_ context.Context, next string) (odp.Page[int], error) {
		calls++
		if next != "/next" {
			t.Fatalf("next = %s", next)
		}
		return odp.Page[int]{Items: []int{3, 4}}, nil
	}) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, item)
	}
	if calls != 1 || !reflect.DeepEqual(got, []int{1, 2, 3, 4}) {
		t.Fatalf("calls %d, items %v", calls, got)
	}
}

func TestPageIterationBoundsUnendingTraversal(t *testing.T) {
	calls, pages, failures := 0, 0, 0
	for _, err := range odp.IteratePages(context.Background(), odp.Page[int]{Next: "/0"}, func(context.Context, string) (odp.Page[int], error) {
		calls++
		return odp.Page[int]{Next: fmt.Sprintf("/%d", calls)}, nil
	}) {
		if err != nil {
			failures++
		} else {
			pages++
		}
	}
	if pages != odp.MaxTraversalPages || failures != 1 || calls > odp.MaxTraversalPages {
		t.Fatalf("pages %d, failures %d, calls %d", pages, failures, calls)
	}
}

func TestResourceIdentityConstruction(t *testing.T) {
	for _, kind := range []odp.ResourceType{odp.ResourceCollection, odp.ResourceOffering} {
		identity, err := odp.NewResourceIdentity("https://EXAMPLE.com:443/.well-known/odp", kind, "search")
		if err != nil {
			t.Fatal(err)
		}
		if identity.Service != "https://example.com" || identity.ID != "search" || identity.Type != kind || identity.Key() != "https://example.com\x00"+string(kind)+"\x00search" {
			t.Fatalf("identity = %#v", identity)
		}
	}
	for _, tc := range []struct {
		origin string
		kind   odp.ResourceType
		id     string
	}{
		{"https://example.com", odp.ResourceOffering, "../search"},
		{"https://example.com", "future", "search"},
		{"http://example.com", odp.ResourceOffering, "search"},
	} {
		if _, err := odp.NewResourceIdentity(tc.origin, tc.kind, tc.id); err == nil {
			t.Fatalf("accepted %#v", tc)
		}
	}
	for _, tc := range []struct {
		value   odp.Optional[int]
		want    int
		present bool
	}{
		{odp.Optional[int]{}, 0, false}, {odp.Null[int](), 0, false}, {odp.Some(0), 0, true}, {odp.Some(42), 42, true},
	} {
		got, ok := tc.value.Get()
		if got != tc.want || ok != tc.present {
			t.Fatalf("Get = %d, %v; want %d, %v", got, ok, tc.want, tc.present)
		}
	}
}
