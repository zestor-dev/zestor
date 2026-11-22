package gomap_test

import (
	"context"
	"errors"
	"testing"

	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/gomap"
)

func TestRejectedWriteLeavesNoEmptyKind(t *testing.T) {
	s := gomap.NewMemStore[int](store.StoreOptions[int]{
		ValidateFns: map[string]store.ValidateFunc[int]{
			"k": func(v int) error {
				if v < 0 {
					return errors.New("neg")
				}
				return nil
			},
		},
	})
	defer s.Close()
	ctx := context.Background()

	if _, err := s.Set(ctx, "k", "a", -1); err == nil {
		t.Fatal("expected validation error")
	}
	all, _ := s.GetAll(ctx)
	if _, ok := all["k"]; ok {
		t.Error("a rejected Set left an empty kind behind")
	}

	if _, err := s.Set(ctx, "k", "a", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Delete(ctx, "k", "a"); err != nil {
		t.Fatal(err)
	}
	all, _ = s.GetAll(ctx)
	if _, ok := all["k"]; ok {
		t.Error("deleting the last key left an empty kind behind")
	}
}
