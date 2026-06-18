package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostPathToContainer(t *testing.T) {
	got := hostPathToContainer("/host", "/root/docker/pxc/data")
	want := "/host/root/docker/pxc/data"
	if got != want {
		t.Fatalf("hostPathToContainer() = %q, want %q", got, want)
	}
}

func TestResolveHostPathAbsoluteSymlink(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "root/docker/pxc"))
	mustMkdirAll(t, filepath.Join(root, "mnt/krystal/docker/pxc/data"))
	if err := os.Symlink("/mnt/krystal/docker/pxc/data", filepath.Join(root, "root/docker/pxc/data")); err != nil {
		t.Fatal(err)
	}

	got := resolveHostPath(root, "/root/docker/pxc/data")
	want := filepath.Join(root, "mnt/krystal/docker/pxc/data")
	if got != want {
		t.Fatalf("resolveHostPath() = %q, want %q", got, want)
	}
}

func TestResolveHostPathRelativeSymlink(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "root/docker/pxc"))
	mustMkdirAll(t, filepath.Join(root, "root/docker/storage/data"))
	if err := os.Symlink("../storage/data", filepath.Join(root, "root/docker/pxc/data")); err != nil {
		t.Fatal(err)
	}

	got := resolveHostPath(root, "/root/docker/pxc/data")
	want := filepath.Join(root, "root/docker/storage/data")
	if got != want {
		t.Fatalf("resolveHostPath() = %q, want %q", got, want)
	}
}

func TestResolveHostPathMissingFinalPath(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "root/docker/pxc"))

	got := resolveHostPath(root, "/root/docker/pxc/data")
	want := filepath.Join(root, "root/docker/pxc/data")
	if got != want {
		t.Fatalf("resolveHostPath() = %q, want %q", got, want)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
