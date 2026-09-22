package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersistsAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := RepositoryState{HeadSHA: "abc", Branch: "main", UpdatedAt: time.Unix(123, 0).UTC()}
	if err := store.Put("api", want); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, ok := reopened.Get("api")
	if !ok || got.HeadSHA != want.HeadSHA || got.Branch != want.Branch || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("state = %#v", got)
	}
}

func TestStoreRejectsASecondProcessOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := Open(path); err == nil {
		t.Fatal("expected a second store owner to be rejected")
	}
}
