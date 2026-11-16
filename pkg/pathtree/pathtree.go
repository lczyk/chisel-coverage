// Package pathtree draws a report's paths as a tree: one line per entry,
// under the directories that hold it, with box drawing below the top level.
package pathtree

import (
	"sort"
	"strings"
)

// Node is a line of a tree and the lines drawn under it.
type Node struct {
	Line string
	Kids []*Node
}

// Draw renders roots with no box drawing and everything under them with it,
// one string per line. The glyphs go through dim; nil leaves them plain.
func Draw(roots []*Node, dim func(string) string) []string {
	if dim == nil {
		dim = func(s string) string { return s }
	}
	var out []string
	var kids func(ns []*Node, prefix string)
	kids = func(ns []*Node, prefix string) {
		for i, n := range ns {
			glyph, pad := "\u251c\u2500 ", "\u2502  "
			if i == len(ns)-1 {
				glyph, pad = "\u2514\u2500 ", "   "
			}
			out = append(out, dim(prefix+glyph)+n.Line)
			kids(n.Kids, prefix+pad)
		}
	}
	for _, r := range roots {
		out = append(out, r.Line)
		kids(r.Kids, "")
	}
	return out
}

// segment is one name of the paths, the entries that end at it and the
// segments under it.
type segment struct {
	name    string
	entries []int
	kids    map[string]*segment
}

// Of is the tree of paths, one line for each. line(i, name) is the line of
// paths[i], given the name the tree draws for it. A segment no path ends at
// is a directory drawn only to hold others, "<name>/", and folds with the
// one under it when that is its only one, so a run of them is one line and
// a path ends the run. A top-level name keeps its leading slash. Siblings
// are sorted by the name drawn, and paths that end at one segment, which a
// caller's normalising can make of two, are sibling lines sorted by path,
// with what is under them drawn under the last. A trailing slash
// is not part of a name, so a directory's path holds what is under it.
func Of(paths []string, line func(i int, name string) string) []*Node {
	root := &segment{kids: map[string]*segment{}}
	for i, p := range paths {
		s := root
		for _, name := range strings.Split(strings.Trim(p, "/"), "/") {
			kid := s.kids[name]
			if kid == nil {
				kid = &segment{name: name, kids: map[string]*segment{}}
				s.kids[name] = kid
			}
			s = kid
		}
		s.entries = append(s.entries, i)
	}
	for name, kid := range root.kids {
		root.kids[name] = fold(kid)
	}
	return nodes(root, "/", paths, line)
}

// fold draws a run of segments with no entry and one kid each as one.
func fold(s *segment) *segment {
	for name, kid := range s.kids {
		s.kids[name] = fold(kid)
	}
	for len(s.entries) == 0 && len(s.kids) == 1 {
		for _, only := range s.kids {
			only.name = s.name + "/" + only.name
			s = only
		}
	}
	return s
}

func nodes(s *segment, lead string, paths []string, line func(int, string) string) []*Node {
	kids := make([]*segment, 0, len(s.kids))
	for _, kid := range s.kids {
		kids = append(kids, kid)
	}
	// By the name drawn, which folding may have made longer than the key.
	sort.Slice(kids, func(i, j int) bool { return kids[i].name < kids[j].name })
	var out []*Node
	for _, kid := range kids {
		name := lead + kid.name
		var last *Node
		if len(kid.entries) == 0 {
			last = &Node{Line: name + "/"}
			out = append(out, last)
		}
		sort.SliceStable(kid.entries, func(i, j int) bool { return paths[kid.entries[i]] < paths[kid.entries[j]] })
		for _, i := range kid.entries {
			last = &Node{Line: line(i, name)}
			out = append(out, last)
		}
		last.Kids = nodes(kid, "", paths, line)
	}
	return out
}
