// Package sdf reads a slice definition file down to what a .deb can answer
// (which paths each slice claims, with their arch lists) and synthesises the
// single-package release file the coverage cuts run against.
package sdf

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Slice names the tool adds to the synthesised file: the whole-deb slice,
// one slice per ignore glob, IgnorePrefix followed by the glob's index, and
// an empty slice whose cut only tells whether the package exists for an
// architecture. Valid chisel slice names, unlikely to clash with a real one.
const (
	AllSlice     = "cov-all"
	IgnorePrefix = "cov-ignore-"
	ProbeSlice   = "cov-probe"
)

// IsIgnore reports whether name is one of the ignore slices.
func IsIgnore(name string) bool { return strings.HasPrefix(name, IgnorePrefix) }

// Definition is the stripped view of a slice definition file.
type Definition struct {
	Package string
	Archive string
	Slices  []Slice // file order
}

// Slice is one slice with only its extractable entries left.
type Slice struct {
	Name    string
	Entries []Entry
}

// Entry is a path a slice claims from the deb, restricted to Arch when set.
type Entry struct {
	Path string
	Arch []string
}

// Slice returns the slice called name, or nil.
func (d *Definition) Slice(name string) *Slice {
	for i := range d.Slices {
		if d.Slices[i].Name == name {
			return &d.Slices[i]
		}
	}
	return nil
}

type yamlPath struct {
	Copy     string    `yaml:"copy"`
	Text     *string   `yaml:"text"`
	Symlink  string    `yaml:"symlink"`
	Make     bool      `yaml:"make"`
	Generate string    `yaml:"generate"`
	Arch     yaml.Node `yaml:"arch"`
}

// Read parses the slice definition file for pkg under release and strips it:
// essentials, mutate scripts, until, generated content and attributes are
// dropped, and a copy entry is re-keyed by its source. Only path keys and their
// arch lists survive. See design.md for why each goes.
func Read(release, pkg string) (*Definition, error) {
	path := filepath.Join(release, "slices", pkg+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	d, err := parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if d.Package != pkg {
		return nil, fmt.Errorf("%s: package is %q, expected %q", path, d.Package, pkg)
	}
	return d, nil
}

func parse(raw []byte) (*Definition, error) {
	var doc struct {
		Package string    `yaml:"package"`
		Archive string    `yaml:"archive"`
		Slices  yaml.Node `yaml:"slices"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Package == "" {
		return nil, fmt.Errorf("no package name")
	}
	d := &Definition{Package: doc.Package, Archive: doc.Archive}
	if doc.Slices.Kind == 0 {
		return d, nil
	}
	if doc.Slices.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("slices is not a mapping")
	}
	for i := 0; i+1 < len(doc.Slices.Content); i += 2 {
		name := doc.Slices.Content[i].Value
		s, err := parseSlice(name, doc.Slices.Content[i+1])
		if err != nil {
			return nil, err
		}
		d.Slices = append(d.Slices, s)
	}
	return d, nil
}

func parseSlice(name string, node *yaml.Node) (Slice, error) {
	s := Slice{Name: name}
	contents := mapValue(node, "contents")
	if contents == nil {
		return s, nil
	}
	if contents.Kind != yaml.MappingNode {
		return s, fmt.Errorf("slice %s: contents is not a mapping", name)
	}
	byPath := map[string]int{}
	for i := 0; i+1 < len(contents.Content); i += 2 {
		path := contents.Content[i].Value
		var p yamlPath
		if err := contents.Content[i+1].Decode(&p); err != nil {
			return s, fmt.Errorf("slice %s: %s: %w", name, path, err)
		}
		if p.Text != nil || p.Symlink != "" || p.Make || p.Generate != "" {
			continue
		}
		if p.Copy != "" {
			path = p.Copy
		}
		arch, err := archList(p.Arch)
		if err != nil {
			return s, fmt.Errorf("slice %s: %s: %w", name, path, err)
		}
		// Re-keying a copy can land on a path the slice also lists directly;
		// the merged entry is as wide as the wider of the two arch lists.
		if j, dup := byPath[path]; dup {
			s.Entries[j].Arch = mergeArch(s.Entries[j].Arch, arch)
			continue
		}
		byPath[path] = len(s.Entries)
		s.Entries = append(s.Entries, Entry{Path: path, Arch: arch})
	}
	return s, nil
}

// archList accepts the scalar and list forms chisel takes for arch.
func archList(n yaml.Node) ([]string, error) {
	switch n.Kind {
	case 0:
		return nil, nil
	case yaml.ScalarNode:
		return []string{n.Value}, nil
	case yaml.SequenceNode:
		var out []string
		for _, c := range n.Content {
			out = append(out, c.Value)
		}
		return out, nil
	}
	return nil, fmt.Errorf("arch is neither a string nor a list")
}

func mergeArch(a, b []string) []string {
	if a == nil || b == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, x := range append(append([]string{}, a...), b...) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// Synthesise renders the release file the cuts run against: the whole-deb
// slice, the slices named in only (every slice when only is nil) stripped as
// read, and one slice per ignore glob. A glob matching nothing fails a cut, so
// the globs are cut apart to tell "matches nothing" from "matches these". It
// also returns the names of the slices worth cutting: a slice with no entries
// left is not cut, its result being empty regardless.
func Synthesise(d *Definition, only []string, ignores []string) (file []byte, cuts []string, err error) {
	root := &yaml.Node{Kind: yaml.MappingNode}
	root.Content = append(root.Content, str("package"), str(d.Package))
	if d.Archive != "" {
		root.Content = append(root.Content, str("archive"), str(d.Archive))
	}
	slices := &yaml.Node{Kind: yaml.MappingNode}
	root.Content = append(root.Content, str("slices"), slices)

	addSlice := func(name string, entries []Entry) {
		contents := &yaml.Node{Kind: yaml.MappingNode}
		for _, e := range entries {
			contents.Content = append(contents.Content, str(e.Path), pathNode(e.Arch))
		}
		slice := &yaml.Node{Kind: yaml.MappingNode}
		slice.Content = append(slice.Content, str("contents"), contents)
		slices.Content = append(slices.Content, str(name), slice)
	}

	addSlice(AllSlice, []Entry{{Path: "/**"}})
	cuts = append(cuts, AllSlice)
	addSlice(ProbeSlice, nil)

	wanted := map[string]bool{}
	for _, name := range only {
		if d.Slice(name) == nil {
			return nil, nil, fmt.Errorf("%s has no slice %q", d.Package, name)
		}
		wanted[name] = true
	}
	for _, s := range d.Slices {
		if only != nil && !wanted[s.Name] {
			continue
		}
		if s.Name == AllSlice || s.Name == ProbeSlice || IsIgnore(s.Name) {
			return nil, nil, fmt.Errorf("%s: slice name %q is reserved by chisel-slice-coverage", d.Package, s.Name)
		}
		if len(s.Entries) > 0 {
			addSlice(s.Name, s.Entries)
			cuts = append(cuts, s.Name)
		}
	}

	for i, g := range ignores {
		name := fmt.Sprintf("%s%d", IgnorePrefix, i)
		addSlice(name, []Entry{{Path: g}})
		cuts = append(cuts, name)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), cuts, nil
}

func pathNode(arch []string) *yaml.Node {
	if arch == nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: ""}
	}
	list := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, a := range arch {
		list.Content = append(list.Content, str(a))
	}
	m := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
	m.Content = append(m.Content, str("arch"), list)
	return m
}

func str(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
