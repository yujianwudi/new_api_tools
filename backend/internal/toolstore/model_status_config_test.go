package toolstore

import (
	"context"
	"errors"
	"testing"
)

func TestGetModelStatusConfigVersionGuardsClosedStoreAndMapsNotFound(t *testing.T) {
	ctx := context.Background()
	var nilStore *Store
	if _, err := nilStore.GetModelStatusConfigVersion(ctx, 0); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("nil store error = %v, want ErrStoreClosed", err)
	}
	if _, err := (&Store{}).GetModelStatusConfigVersion(ctx, 1); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("store without database error = %v, want ErrStoreClosed", err)
	}

	store, _ := newTestStore(t)
	if _, err := store.GetModelStatusConfigVersion(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version error = %v, want ErrNotFound", err)
	}
}
