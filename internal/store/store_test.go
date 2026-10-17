package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestPersistentTransactionsAndSeparateHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = a.Update(func(s *State) error { s.Principals["first"] = Principal{ID: "first"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = b.Update(func(s *State) error { s.Principals["second"] = Principal{ID: "second"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(a.View().Principals) != 2 {
		t.Fatal("concurrent provisioner lost a principal")
	}
	if err = a.Update(func(s *State) error { delete(s.Principals, "first"); return errors.New("abort") }); err == nil {
		t.Fatal("transaction unexpectedly committed")
	}
	if len(b.View().Principals) != 2 {
		t.Fatal("failed transaction changed stored state")
	}
}
