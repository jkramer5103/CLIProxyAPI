package auth

import (
	"context"
	"errors"
	"testing"
)

func TestAccountOperationRejectsRemovedAndCanceledCredentials(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a, err := m.Register(context.Background(), &Auth{ID: "account", Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.WithAccountOperation(context.Background(), a.ID, func(current *Auth) error {
		if current.ID != a.ID {
			t.Fatal("wrong snapshot")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = m.WithAccountOperation(ctx, a.ID, func(*Auth) error { t.Fatal("canceled callback ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	m.Remove(context.Background(), a.ID)
	if err = m.WithAccountOperation(context.Background(), a.ID, func(*Auth) error { t.Fatal("removed callback ran"); return nil }); err == nil {
		t.Fatal("removed account accepted")
	}
}
