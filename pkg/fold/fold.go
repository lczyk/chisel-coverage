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
// package names, java, go, dotnet, libunwind, ruby and the kernel's arch
// directories call it. x86 is both amd64's and i386's, as in the kernel
// headers' arch/x86.
var words = map[string][]string{
	"amd64":   {"amd64", "x86_64", "x86-64", "x64", "x86"},
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
// the items to fold (a state, a description). Raw and RawTag are the
// architecture's own forms of the two, which fold too when they are the
// same everywhere; a RawTag of "" is the Tag itself.
type Item struct {
	Key    string
	Tag    string
	Raw    string
	RawTag string
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
	arch, key, raw, tag, rawTag string
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
// belongs to arches[i]. Items fold when their keys are equal or their raw
// forms are, and their tags are equal or their raw tags are, and what any
// of that joins is one entry: a path that is the same word for word on
// every architecture folds whatever Normalise made of it, so a spelling in
// Normalise can only ever merge. The entry's key and tag are the
// normalised ones where a member's raw form differs from them, else the
// raw forms themselves. Should both ways of folding pull
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
	// Every form of an item is one handle, and all of an item's handles
	// are joined: the key with the tag, the raw form with the raw tag, and
	// the two cross pairs.
	handles := func(it Item) []string {
		rawTag := it.RawTag
		if rawTag == "" {
			rawTag = it.Tag
		}
		return []string{
			"kt\x00" + it.Key + "\x00" + it.Tag,
			"kr\x00" + it.Key + "\x00" + rawTag,
			"rt\x00" + it.Raw + "\x00" + it.Tag,
			"rr\x00" + it.Raw + "\x00" + rawTag,
		}
	}
	for i := range arches {
		for _, it := range items[i] {
			h := handles(it)
			for _, x := range h[1:] {
				union(h[0], x)
			}
		}
	}
	byRoot := map[string][]member{}
	var roots []string
	for i, a := range arches {
		for _, it := range items[i] {
			root := find(handles(it)[0])
			if byRoot[root] == nil {
				roots = append(roots, root)
			}
			rawTag := it.RawTag
			if rawTag == "" {
				rawTag = it.Tag
			}
			byRoot[root] = append(byRoot[root], member{a, it.Key, it.Raw, it.Tag, rawTag})
		}
	}
	var entries []struct {
		arches []string
		entry  Entry
	}
	emit := func(members []member) {
		var e Entry
		var arches []string
		for _, m := range members {
			arches = append(arches, m.arch)
			e.Raw = append(e.Raw, m.raw)
			if m.key != m.raw && (e.Key == "" || m.key < e.Key) {
				e.Key = m.key
			}
			if m.tag != m.rawTag && (e.Tag == "" || m.tag < e.Tag) {
				e.Tag = m.tag
			}
		}
		if e.Key == "" {
			e.Key = members[0].raw
		}
		if e.Tag == "" {
			e.Tag = members[0].rawTag
		}
		entries = append(entries, struct {
			arches []string
			entry  Entry
		}{arches, e})
	}
	for _, root := range roots {
		members := byRoot[root]
		if !hasTwice(members) {
			emit(members)
			continue
		}
		// Keys alone, and where a key still holds one architecture twice
		// (the x32 abi folds onto gnu), the k-th item of every
		// architecture makes the k-th entry, so an entry never lists an
		// architecture twice and the first ones still fold.
		for _, byKey := range partition(members, func(m member) string { return m.key + "\x00" + m.tag }) {
			nth := map[string]int{}
			for _, byN := range partition(byKey, func(m member) string {
				nth[m.arch]++
				return strconv.Itoa(nth[m.arch])
			}) {
				emit(byN)
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
