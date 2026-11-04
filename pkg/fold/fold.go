// Package fold compares what architectures have and groups them on it:
// paths are normalised so that what names the architecture compares equal,
// and items are keyed by the set of architectures that share them, so a
// report can show one block for every architecture that agrees.
package fold

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/lczyk/chisel-proxy/pkg/deb"
)

// words is what names an architecture as a word of a path: its own name,
// the cpu of its multiarch triplet, and what debian's cross toolchain
// package names, java, go, dotnet, libunwind and ruby call it.
var words = map[string][]string{
	"amd64":   {"amd64", "x86_64", "x86-64", "x64"},
	"arm64":   {"arm64", "aarch64"},
	"armhf":   {"armhf", "arm"},
	"i386":    {"i386", "i686", "x86", "386"},
	"ppc64el": {"ppc64el", "powerpc64le", "ppc64le", "ppc64"},
	"riscv64": {"riscv64", "riscv"},
	"s390x":   {"s390x", "s390"},
}

// anyWord matches every architecture's words, longest first, so that x86_64
// is amd64's word and not i386's x86 followed by _64.
var anyWord = func() *regexp.Regexp {
	var all []string
	for _, ws := range words {
		all = append(all, ws...)
	}
	sort.Slice(all, func(i, j int) bool { return len(all[i]) > len(all[j]) })
	return regexp.MustCompile(strings.Join(all, "|"))
}()

var abi = regexp.MustCompile(`<arch>-linux-gnu[a-z0-9]*`)

// Normalise rewrites what names the architecture in path, so paths from
// different architectures compare equal: every word of the path that is
// arch's (x86_64, amd64, x64 on amd64; i386, i686, x86 and 386 on i386)
// becomes <arch>, as in x86_64-linux-gnu, java-21-openjdk-amd64,
// jre/lib/aarch64, linux_386 or README.Debian.armhf.gz, and the abi part of
// a multiarch triplet becomes <gnu>, in file names too. A word is bounded
// by anything but a letter or digit, so arm inside apparmor stays, and a
// word of another architecture (x86_64 on i386) is left alone. A path of
// an architecture chisel does not know is returned as it is.
func Normalise(path, arch string) string {
	own := words[arch]
	if own == nil {
		if !deb.ValidArch(arch) {
			return path
		}
		own = []string{arch}
	}
	var b strings.Builder
	last := 0
	for _, m := range anyWord.FindAllStringIndex(path, -1) {
		i, j := m[0], m[1]
		if i > 0 && alnum(path[i-1]) || j < len(path) && alnum(path[j]) {
			continue
		}
		if !contains(own, path[i:j]) {
			continue
		}
		b.WriteString(path[last:i])
		b.WriteString("<arch>")
		last = j
	}
	b.WriteString(path[last:])
	return abi.ReplaceAllString(b.String(), "<arch>-linux-<gnu>")
}

func contains(ws []string, w string) bool {
	for _, x := range ws {
		if x == w {
			return true
		}
	}
	return false
}

func alnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// Item is one thing an architecture has. Key and Tag together say which
// items of different architectures are the same: Key is the normalised
// form (a path after Normalise, say) and Tag whatever else must agree for
// the items to fold (a state, a description). Raw is the architecture's own
// form of the item, which folds too when it is the same everywhere.
type Item struct {
	Key string
	Tag string
	Raw string
}

// Entry is one key and tag every architecture of a group has, with each
// architecture's raw form in the group's architecture order.
type Entry struct {
	Key string
	Tag string
	Raw []string
}

// Shown is what a report prints for the entry: the raw form when it is the
// same on every architecture, the normalised key when they differ.
func (e Entry) Shown() string {
	for _, r := range e.Raw[1:] {
		if r != e.Raw[0] {
			return e.Key
		}
	}
	return e.Raw[0]
}

// Group is the set of architectures that share some items and those items,
// sorted by key then tag.
type Group struct {
	Arches  []string
	Entries []Entry
}

type member struct {
	arch, key, raw string
}

// partition splits the members by what by returns, in first-seen order.
func partition(members []member, by func(member) string) [][]member {
	index := map[string]int{}
	var out [][]member
	for _, m := range members {
		k := by(m)
		i, ok := index[k]
		if !ok {
			i = len(out)
			index[k] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], m)
	}
	return out
}

// hasTwice reports whether an architecture appears twice among members.
func hasTwice(members []member) bool {
	seen := map[string]bool{}
	for _, m := range members {
		if seen[m.arch] {
			return true
		}
		seen[m.arch] = true
	}
	return false
}

// Fold groups the items by the architectures that have them: items[i]
// belongs to arches[i]. Items with the same tag fold when their keys are
// equal or their raw forms are, and what either joins is one entry: a path
// that is the same word for word on every architecture folds whatever
// Normalise made of it, so a spelling in Normalise can only ever merge.
// The entry's key is the normalised one where a member's raw form differs
// from its key, else the raw form itself. Should both ways of folding pull
// two items of one architecture into one entry, that entry is folded on
// keys alone instead. Every distinct set of architectures is one group,
// most architectures first, then in the order the architectures were
// given. An architecture's groups together hold every item it had.
func Fold(arches []string, items [][]Item) []Group {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b string) {
		if _, ok := parent[a]; !ok {
			parent[a] = a
		}
		if _, ok := parent[b]; !ok {
			parent[b] = b
		}
		parent[find(a)] = find(b)
	}
	for i := range arches {
		for _, it := range items[i] {
			union("k\x00"+it.Key+"\x00"+it.Tag, "r\x00"+it.Raw+"\x00"+it.Tag)
		}
	}
	type folded struct {
		tag     string
		members []member
	}
	byRoot := map[string]*folded{}
	var roots []string
	for i, a := range arches {
		for _, it := range items[i] {
			root := find("k\x00" + it.Key + "\x00" + it.Tag)
			f := byRoot[root]
			if f == nil {
				f = &folded{tag: it.Tag}
				byRoot[root] = f
				roots = append(roots, root)
			}
			f.members = append(f.members, member{a, it.Key, it.Raw})
		}
	}
	var entries []struct {
		arches []string
		entry  Entry
	}
	emit := func(tag string, members []member) {
		e := Entry{Tag: tag}
		var arches []string
		for _, m := range members {
			arches = append(arches, m.arch)
			e.Raw = append(e.Raw, m.raw)
			if m.key != m.raw && (e.Key == "" || m.key < e.Key) {
				e.Key = m.key
			}
		}
		if e.Key == "" {
			e.Key = members[0].raw
		}
		entries = append(entries, struct {
			arches []string
			entry  Entry
		}{arches, e})
	}
	for _, root := range roots {
		f := byRoot[root]
		if !hasTwice(f.members) {
			emit(f.tag, f.members)
			continue
		}
		// Keys alone, and where a key still holds one architecture twice
		// (the x32 abi folds onto gnu), the k-th item of every
		// architecture makes the k-th entry, so an entry never lists an
		// architecture twice and the first ones still fold.
		for _, byKey := range partition(f.members, func(m member) string { return m.key }) {
			nth := map[string]int{}
			for _, byN := range partition(byKey, func(m member) string {
				nth[m.arch]++
				return strconv.Itoa(nth[m.arch])
			}) {
				emit(f.tag, byN)
			}
		}
	}
	// By what a report shows, so the blocks read sorted.
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].entry, entries[j].entry
		if sa, sb := a.Shown(), b.Shown(); sa != sb {
			return sa < sb
		}
		return a.Tag < b.Tag
	})
	index := map[string]int{}
	var groups []Group
	for _, e := range entries {
		set := strings.Join(e.arches, "\x00")
		i, ok := index[set]
		if !ok {
			i = len(groups)
			index[set] = i
			groups = append(groups, Group{Arches: e.arches})
		}
		groups[i].Entries = append(groups[i].Entries, e.entry)
	}
	rank := map[string]int{}
	for i, a := range arches {
		rank[a] = i
	}
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := groups[i].Arches, groups[j].Arches
		if len(a) != len(b) {
			return len(a) > len(b)
		}
		for k := range a {
			if a[k] != b[k] {
				return rank[a[k]] < rank[b[k]]
			}
		}
		return false
	})
	return groups
}
