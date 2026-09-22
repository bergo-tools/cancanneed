package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStorePersistsAtomically(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested", "state")
	store, err := Open(directory, "api")
	if err != nil {
		t.Fatal(err)
	}
	want := RepositoryState{HeadSHA: "abc", Branch: "main", UpdatedAt: time.Unix(123, 0).UTC()}
	if err := store.Put("api", want); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "api.json")); err != nil {
		t.Fatalf("repository state file missing: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory, "api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, ok := reopened.Get("api")
	if !ok || got.HeadSHA != want.HeadSHA || got.Branch != want.Branch || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("state = %#v", got)
	}
}

func TestStoreLocksAndWritesRepositoriesIndependently(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	api, err := Open(directory, "api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	web, err := Open(directory, "web")
	if err != nil {
		t.Fatalf("independent repository lock failed: %v", err)
	}
	t.Cleanup(func() { _ = web.Close() })
	if _, err := Open(directory, "api"); err == nil {
		t.Fatal("expected the same repository state to remain exclusively locked")
	}
	if err := api.Put("api", RepositoryState{HeadSHA: "api-head"}); err != nil {
		t.Fatal(err)
	}
	if err := web.Put("web", RepositoryState{HeadSHA: "web-head"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"api", "web"} {
		if _, err := os.Stat(filepath.Join(directory, name+".json")); err != nil {
			t.Fatalf("%s state file missing: %v", name, err)
		}
	}
}

func TestRepositoryFileNameIsReadableAndCollisionResistant(t *testing.T) {
	if got := RepositoryFileName("backend"); got != "backend.json" {
		t.Fatalf("simple filename = %q", got)
	}
	first := RepositoryFileName("team/backend")
	second := RepositoryFileName("team backend")
	if first == second || !strings.HasPrefix(first, "team-backend-") || !strings.HasSuffix(first, ".json") {
		t.Fatalf("unsafe filenames = %q and %q", first, second)
	}
}

func TestStoreRejectsASecondProcessOwner(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	store, err := Open(directory, "api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := Open(directory, "api"); err == nil {
		t.Fatal("expected a second store owner to be rejected")
	}
}
