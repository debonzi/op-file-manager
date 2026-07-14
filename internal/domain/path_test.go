package domain

import (
	"strings"
	"testing"
)

func TestValidateRemotePath(t *testing.T) {
	parts, err := ValidateRemotePath("project/dev/.env")
	if err != nil {
		t.Fatalf("ValidateRemotePath() error = %v", err)
	}
	if got, want := len(parts), 3; got != want || parts[2] != ".env" {
		t.Fatalf("ValidateRemotePath() = %#v", parts)
	}
	for _, invalid := range []string{"", "/absolute", "project//.env", "project/../.env", "project/./.env", "line\nfeed"} {
		if _, err := ValidateRemotePath(invalid); err == nil {
			t.Errorf("ValidateRemotePath(%q) succeeded", invalid)
		}
	}
}

func TestBuildTreeMovesDuplicateAndInvalidTitlesToReadOnlyEntries(t *testing.T) {
	tree := BuildTree([]Document{
		{ID: "one", Title: "project/.env"},
		{ID: "two", Title: "project/.env"},
		{ID: "three", Title: "../outside"},
	})
	project := tree.Entries(nil)
	if len(project) != 3 { // project folder plus two invalid entries at root
		t.Fatalf("root entries = %#v", project)
	}
	entries := tree.Entries([]string{"project"})
	if len(entries) != 1 || entries[0].Document == nil || entries[0].Document.ID != "one" {
		t.Fatalf("project entries = %#v", entries)
	}
	if got := tree.FindDocument("project/.env"); got == nil || got.ID != "one" {
		t.Fatalf("FindDocument() = %#v", got)
	}
}

func TestJoinRemotePathEnforcesTitleLimit(t *testing.T) {
	long := strings.Repeat("a", MaxRemotePathRunes+1)
	if _, err := JoinRemotePath(nil, long); err == nil {
		t.Fatal("JoinRemotePath accepted a path over the configured limit")
	}
}

func TestDirectoryMarkerCreatesVisibleEmptyDirectoryWithoutListingMarker(t *testing.T) {
	tree := BuildTree([]Document{{
		ID:    "marker",
		Title: "projects/api/",
		Tags:  []string{DirectoryMarkerTag},
	}})
	root := tree.Entries(nil)
	if len(root) != 1 || !root[0].IsDir || root[0].Name != "projects" {
		t.Fatalf("root entries = %#v", root)
	}
	entries := tree.Entries([]string{"projects", "api"})
	if len(entries) != 0 {
		t.Fatalf("empty marker directory entries = %#v", entries)
	}
	marker := tree.EmptyDirectoryMarker([]string{"projects", "api"})
	if marker == nil || marker.ID != "marker" {
		t.Fatalf("EmptyDirectoryMarker() = %#v", marker)
	}
}

func TestDirectoryMarkerMustHaveDedicatedTag(t *testing.T) {
	tree := BuildTree([]Document{{ID: "document", Title: "projects/api/"}})
	entries := tree.Entries(nil)
	if len(entries) != 1 || !entries[0].Invalid || entries[0].Name != "projects/api/" {
		t.Fatalf("root entries = %#v", entries)
	}
}

func TestEmptyDirectoryMarkerRejectsDirectoriesWithChildren(t *testing.T) {
	tree := BuildTree([]Document{
		{ID: "marker", Title: "projects/", Tags: []string{DirectoryMarkerTag}},
		{ID: "file", Title: "projects/.env"},
	})
	if marker := tree.EmptyDirectoryMarker([]string{"projects"}); marker != nil {
		t.Fatalf("EmptyDirectoryMarker() = %#v, want nil", marker)
	}
	if marker := tree.DirectoryMarker([]string{"projects"}); marker == nil || marker.ID != "marker" {
		t.Fatalf("DirectoryMarker() = %#v", marker)
	}
}

func TestDirectoryMarkerTitleReservesTrailingSlash(t *testing.T) {
	if _, err := DirectoryMarkerTitle(strings.Repeat("a", MaxRemotePathRunes)); err == nil {
		t.Fatal("DirectoryMarkerTitle accepted a title without room for its trailing slash")
	}
	title, err := DirectoryMarkerTitle(strings.Repeat("a", MaxRemotePathRunes-1))
	if err != nil || !strings.HasSuffix(title, "/") {
		t.Fatalf("DirectoryMarkerTitle() = %q, %v", title, err)
	}
}
