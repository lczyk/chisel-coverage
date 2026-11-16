package pathtree

import (
	"strings"
	"testing"

	"github.com/lczyk/assert"
)

// boxes spells the glyphs in ascii in a test's expected text: "T- ", "L- "
// and "|  " for the three Draw draws.
func boxes(s string) string {
	return strings.NewReplacer("T- ", "\u251c\u2500 ", "L- ", "\u2514\u2500 ", "|  ", "\u2502  ").Replace(s)
}

// draw is the tree of paths, each line its name with a mark of its own.
func draw(paths ...string) string {
	line := func(i int, name string) string {
		if strings.HasSuffix(paths[i], "/") {
			name += "/"
		}
		return name + " #" + string(rune('a'+i))
	}
	return strings.Join(Draw(Of(paths, line), nil), "\n") + "\n"
}

func TestOf(t *testing.T) {
	assert.Equal(t, draw(
		"/etc/x",
		"/usr/bin/new",
		"/usr/bin/su",
		"/usr/lib/libA.so.1",
		"/usr/lib/libB.so.1",
		"/usr/share/doc/liba/copyright",
		"/usr/share/doc/libb/copyright",
		// A directory with entries under it: an entry stops the fold.
		"/var/run/",
		"/var/run/x",
	), boxes(`/etc/x #a
/usr/
T- bin/
|  T- new #b
|  L- su #c
T- lib/
|  T- libA.so.1 #d
|  L- libB.so.1 #e
L- share/doc/
   T- liba/copyright #f
   L- libb/copyright #g
/var/run/ #h
L- x #i
`))
}

func TestOfOrder(t *testing.T) {
	// By the name drawn: /a/z folded is after /a-b.
	assert.Equal(t, draw("/a/z", "/a-b"), "/a-b #b\n/a/z #a\n")
	// Siblings in order whatever the order of the paths.
	assert.Equal(t, draw("/d/c", "/d/a", "/d/b"), boxes("/d/\nT- a #b\nT- b #c\nL- c #a\n"))
}

// Two paths that end at one segment are two lines, sorted by path, and
// what is under them is drawn under the last.
func TestOfTwice(t *testing.T) {
	assert.Equal(t, draw("/d/x/", "/d/x", "/d/x/y", "/d/z"), boxes(`/d/
T- x #b
T- x/ #a
|  L- y #c
L- z #d
`))
}

func TestOfEmpty(t *testing.T) {
	assert.Equal(t, len(Of(nil, nil)), 0)
	assert.Equal(t, len(Draw(nil, nil)), 0)
}

// Every path is a line of its own, and every line but a directory drawn
// only to hold others is a path.
func TestOfOneLineEach(t *testing.T) {
	paths := []string{"/a/b/c", "/a/b/d", "/a/e", "/f", "/g/h/i/j", "/g/h/k"}
	n := 0
	for _, l := range Draw(Of(paths, func(i int, name string) string { return name + " ." }), nil) {
		if strings.HasSuffix(l, " .") {
			n++
		}
	}
	assert.Equal(t, n, len(paths))
}

func TestDraw(t *testing.T) {
	roots := []*Node{
		{Line: "r", Kids: []*Node{
			{Line: "a", Kids: []*Node{{Line: "b"}}},
			{Line: "c", Kids: []*Node{{Line: "d"}}},
		}},
		{Line: "s"},
	}
	assert.Equal(t, strings.Join(Draw(roots, nil), "\n"), boxes("r\nT- a\n|  L- b\nL- c\n   L- d\ns"))
	// The glyphs, and the bars before them, go through dim as one.
	dim := func(s string) string { return "<" + s + ">" }
	assert.Equal(t, strings.Join(Draw(roots, dim), "\n"), boxes("r\n<T- >a\n<|  L- >b\n<L- >c\n<   L- >d\ns"))
}
