// Package fold compares what architectures have and groups them on it:
// paths are normalised so that what names the architecture compares equal,
// and items are keyed by the set of architectures that share them, so a
// report can show one block for every architecture that agrees.
package fold

import (
	"regexp"
	"sort"
	"strings"

	"github.com/lczyk/chisel-proxy/pkg/deb"
)

var abi = regexp.MustCompile(`<arch>-linux-gnu[a-z0-9]*`)

// extraCPUs is what names an architecture's cpu in paths besides the deb
// spellings: ruby's RbConfig says x86 for i386.
var extraCPUs = map[string][]string{
	"i386": {"x86"},
}

// Normalise rewrites what names the architecture in path, so paths from
// different architectures compare equal: the multiarch triplet becomes
// <arch>-linux-<gnu> (the cpu part only where -linux- follows, so "arm" inside
// "apparmor" is left alone, in every spelling the architecture's cpu goes by;
// the abi part wherever it is, in file names too),
// and the architecture's own name becomes <arch> where it stands as a word,
// as in java-21-openjdk-amd64 or README.Debian.armhf.gz.
func Normalise(path, arch string) string {
	cpus := deb.CPUs(arch)
	if cpus == nil {
		return path
	}
	for _, cpu := range append(cpus, extraCPUs[arch]...) {
		path = strings.ReplaceAll(path, cpu+"-linux-", "<arch>-linux-")
	}
	path = abi.ReplaceAllString(path, "<arch>-linux-<gnu>")
	return replaceWord(path, arch, "<arch>")
}

// replaceWord replaces word in s where neither neighbour is a letter or digit.
func replaceWord(s, word, with string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, word)
		if i < 0 {
			break
		}
		j := i + len(word)
		if (i == 0 || !alnum(s[i-1])) && (j == len(s) || !alnum(s[j])) {
			b.WriteString(s[:i])
			b.WriteString(with)
		} else {
			b.WriteString(s[:j])
		}
		s = s[j:]
	}
	b.WriteString(s)
	return b.String()
}

func alnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// Item is one thing an architecture has. Key and Tag together say which
// items of different architectures are the same: Key is the normalised
// form (a path after Normalise, say) and Tag whatever else must agree for
// the items to fold (a state, a description). Raw is the architecture's own
// form of the item, shown when it stands alone.
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

// Shown is what a report prints for the entry: the raw form when one
// architecture has it, the normalised key when several do.
func (e Entry) Shown() string {
	if len(e.Raw) == 1 {
		return e.Raw[0]
	}
	return e.Key
}

// Group is the set of architectures that share some items and those items,
// sorted by key then tag.
type Group struct {
	Arches  []string
	Entries []Entry
}

// Fold groups the items by the architectures that have them: items[i]
// belongs to arches[i], items with the same key and tag fold, and every
// distinct set of architectures is one group. Groups come most
// architectures first, then in the order the architectures were given.
// An architecture's groups together hold every item it had.
func Fold(arches []string, items [][]Item) []Group {
	type folded struct {
		arches []string
		raw    []string
	}
	entries := map[string]*folded{}
	var order []string
	for i, a := range arches {
		for _, it := range items[i] {
			key := it.Key + "\x00" + it.Tag
			e := entries[key]
			if e == nil {
				e = &folded{}
				entries[key] = e
				order = append(order, key)
			}
			e.arches = append(e.arches, a)
			e.raw = append(e.raw, it.Raw)
		}
	}
	sort.Strings(order)
	index := map[string]int{}
	var groups []Group
	for _, key := range order {
		e := entries[key]
		set := strings.Join(e.arches, "\x00")
		i, ok := index[set]
		if !ok {
			i = len(groups)
			index[set] = i
			groups = append(groups, Group{Arches: e.arches})
		}
		k, tag, _ := strings.Cut(key, "\x00")
		groups[i].Entries = append(groups[i].Entries, Entry{Key: k, Tag: tag, Raw: e.raw})
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
