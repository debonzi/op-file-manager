// Package domain defines the filesystem-independent remote model.
package domain

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxRemotePathRunes      = 200
	DirectoryMarkerTag      = "opfm:directory-marker"
	DirectoryMarkerFileName = ".opfm-directory"
)

// Document is the metadata required to navigate and transfer a 1Password
// Document. Content is intentionally never represented in the application.
type Document struct {
	ID        string
	Title     string
	FileName  string
	Tags      []string
	CreatedAt string
	UpdatedAt string
	Size      int64
}

// ValidateRemotePath validates the title convention used by opfm.
func ValidateRemotePath(remotePath string) ([]string, error) {
	if remotePath == "" {
		return nil, errors.New("path is empty")
	}
	if !utf8.ValidString(remotePath) {
		return nil, errors.New("path is not valid UTF-8")
	}
	if utf8.RuneCountInString(remotePath) > MaxRemotePathRunes {
		return nil, fmt.Errorf("path exceeds %d characters", MaxRemotePathRunes)
	}
	if strings.HasPrefix(remotePath, "/") {
		return nil, errors.New("path must be relative")
	}
	for _, character := range remotePath {
		if unicode.IsControl(character) {
			return nil, errors.New("path contains a control character")
		}
	}
	if path.Clean(remotePath) != remotePath {
		return nil, errors.New("path is not normalized")
	}

	parts := strings.Split(remotePath, "/")
	for _, segment := range parts {
		if segment == "" || segment == "." || segment == ".." {
			return nil, errors.New("path contains an invalid segment")
		}
	}
	return parts, nil
}

func JoinRemotePath(dir []string, name string) (string, error) {
	if strings.Contains(name, "/") || name == "" {
		return "", errors.New("file name must be a single path segment")
	}
	remotePath := strings.Join(append(append([]string(nil), dir...), name), "/")
	_, err := ValidateRemotePath(remotePath)
	return remotePath, err
}

// DirectoryMarkerTitle returns the title used for opfm's empty-directory
// marker documents. A trailing slash makes the marker distinct from files.
func DirectoryMarkerTitle(dirPath string) (string, error) {
	if _, err := ValidateRemotePath(dirPath); err != nil {
		return "", err
	}
	if utf8.RuneCountInString(dirPath)+1 > MaxRemotePathRunes {
		return "", fmt.Errorf("directory path exceeds %d characters", MaxRemotePathRunes-1)
	}
	return dirPath + "/", nil
}

// ParseDirectoryMarker recognizes only documents created with the dedicated
// marker tag. A document whose title merely ends in a slash remains visible as
// an invalid regular document rather than being hidden accidentally.
func ParseDirectoryMarker(doc Document) ([]string, bool) {
	if !hasTag(doc.Tags, DirectoryMarkerTag) || !strings.HasSuffix(doc.Title, "/") {
		return nil, false
	}
	dirPath := strings.TrimSuffix(doc.Title, "/")
	if dirPath == "" {
		return nil, false
	}
	if _, err := DirectoryMarkerTitle(dirPath); err != nil {
		return nil, false
	}
	parts, err := ValidateRemotePath(dirPath)
	if err != nil {
		return nil, false
	}
	return parts, true
}

func hasTag(tags []string, wanted string) bool {
	for _, tag := range tags {
		if tag == wanted {
			return true
		}
	}
	return false
}

func LeafName(remotePath string) string {
	return path.Base(remotePath)
}

type Tree struct {
	root    *Node
	invalid []Document
}

type Node struct {
	Name     string
	Children map[string]*Node
	Document *Document
	Marker   *Document
}

type Entry struct {
	Name     string
	IsDir    bool
	Document *Document
	Marker   *Document
	Invalid  bool
}

// BuildTree projects valid titles into a directory tree. Documents that don't
// conform to the convention, or share a title, remain visible as invalid and
// are deliberately not writable by the TUI.
func BuildTree(documents []Document) Tree {
	tree := Tree{root: &Node{Children: map[string]*Node{}}}
	for i := range documents {
		doc := documents[i]
		if _, isMarker := ParseDirectoryMarker(doc); isMarker {
			continue
		}
		parts, err := ValidateRemotePath(doc.Title)
		if err != nil {
			tree.invalid = append(tree.invalid, doc)
			continue
		}

		current := tree.root
		collision := false
		for index, part := range parts {
			child := current.Children[part]
			if child == nil {
				child = &Node{Name: part, Children: map[string]*Node{}}
				current.Children[part] = child
			}
			if index == len(parts)-1 {
				if child.Document != nil || len(child.Children) > 0 {
					collision = true
					break
				}
				child.Document = &doc
			}
			if child.Document != nil && index != len(parts)-1 {
				collision = true
				break
			}
			current = child
		}
		if collision {
			tree.invalid = append(tree.invalid, doc)
		}
	}
	for i := range documents {
		doc := documents[i]
		parts, isMarker := ParseDirectoryMarker(doc)
		if !isMarker {
			continue
		}

		current := tree.root
		collision := false
		for index, part := range parts {
			if current.Document != nil {
				collision = true
				break
			}
			child := current.Children[part]
			if child == nil {
				child = &Node{Name: part, Children: map[string]*Node{}}
				current.Children[part] = child
			}
			if child.Document != nil {
				collision = true
				break
			}
			if index == len(parts)-1 {
				child.Marker = &doc
			}
			current = child
		}
		if collision {
			continue
		}
	}
	return tree
}

func (t Tree) Entries(dir []string) []Entry {
	current := t.root
	for _, segment := range dir {
		child := current.Children[segment]
		if child == nil || child.Document != nil {
			return nil
		}
		current = child
	}

	entries := make([]Entry, 0, len(current.Children)+1)
	for _, child := range current.Children {
		entries = append(entries, Entry{Name: child.Name, IsDir: child.Document == nil, Document: child.Document, Marker: child.Marker})
	}
	if len(dir) == 0 {
		for _, doc := range t.invalid {
			name := doc.Title
			if name == "" {
				name = "(untitled)"
			}
			entries = append(entries, Entry{Name: name, Document: &doc, Invalid: true})
		}
	}
	SortEntries(entries)
	return entries
}

// DirectoryMarker returns the explicit marker for a virtual directory, if it
// has one. Implied directories created by a file path return nil.
func (t Tree) DirectoryMarker(dir []string) *Document {
	current := t.root
	for _, segment := range dir {
		current = current.Children[segment]
		if current == nil || current.Document != nil {
			return nil
		}
	}
	return current.Marker
}

// EmptyDirectoryMarker returns a marker only when its directory has no
// children, making it safe to remove without affecting normal documents.
func (t Tree) EmptyDirectoryMarker(dir []string) *Document {
	current := t.root
	for _, segment := range dir {
		current = current.Children[segment]
		if current == nil || current.Document != nil {
			return nil
		}
	}
	if len(current.Children) != 0 {
		return nil
	}
	return current.Marker
}

// FindDocument returns a valid, uniquely-addressable document by virtual path.
func (t Tree) FindDocument(remotePath string) *Document {
	parts, err := ValidateRemotePath(remotePath)
	if err != nil {
		return nil
	}
	current := t.root
	for _, segment := range parts {
		current = current.Children[segment]
		if current == nil {
			return nil
		}
	}
	return current.Document
}

func SortEntries(entries []Entry) {
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			left, right := entries[i], entries[j]
			if (!left.IsDir && right.IsDir) || (left.IsDir == right.IsDir && strings.ToLower(left.Name) > strings.ToLower(right.Name)) {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
}
