package localfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootConfinesPathsAndRejectsSymlinkSources(t *testing.T) {
	directory := t.TempDir()
	root, err := NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "secret.env")
	if err := os.WriteFile(file, []byte("KEY=value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.CheckSourceFile(file); err != nil {
		t.Fatalf("CheckSourceFile() error = %v", err)
	}
	if _, err := root.Resolve("../outside"); err == nil {
		t.Fatal("Resolve() accepted a path outside the root")
	}
	link := filepath.Join(directory, "linked.env")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if err := root.CheckSourceFile(link); err == nil {
		t.Fatal("CheckSourceFile() accepted a symbolic link")
	}
}

func TestCheckDestinationRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	root, err := NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "download.env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	exists, err := root.CheckDestination(link)
	if !exists || err == nil {
		t.Fatalf("CheckDestination() = (%v, %v), want existing symlink error", exists, err)
	}
}

func TestListIncludesMetadataForTableRendering(t *testing.T) {
	directory := t.TempDir()
	root, err := NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "secret.env"), []byte("KEY=value"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := root.List(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Size != int64(len("KEY=value")) || entries[0].Modified.IsZero() || !entries[0].Mode.IsRegular() {
		t.Fatalf("List() = %#v", entries)
	}
}
