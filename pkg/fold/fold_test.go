package fold

import (
	"testing"

	"github.com/lczyk/assert"
)

func TestNormalise(t *testing.T) {
	for _, tc := range []struct{ path, arch, want string }{
		{"/usr/lib/x86_64-linux-gnu/libc.so.6", "amd64", "/usr/lib/<arch>-linux-<gnu>/libc.so.6"},
		{"/usr/lib/aarch64-linux-gnu/", "arm64", "/usr/lib/<arch>-linux-<gnu>/"},
		{"/usr/lib/arm-linux-gnueabihf/bash/", "armhf", "/usr/lib/<arch>-linux-<gnu>/bash/"},
		{"/usr/bin/x86_64-linux-gnu-gcc", "amd64", "/usr/bin/<arch>-linux-<gnu>-gcc"},
		{"/usr/share/man/man1/perl5.40-arm-linux-gnueabihf.1.gz", "armhf", "/usr/share/man/man1/perl5.40-<arch>-linux-<gnu>.1.gz"},
		{"/usr/lib/python3.14/lib-dynload/_ssl.cpython-314-x86_64-linux-gnu.so", "amd64", "/usr/lib/python3.14/lib-dynload/_ssl.cpython-314-<arch>-linux-<gnu>.so"},
		{"/usr/lib/gcc/i686-linux-gnu/13/", "i386", "/usr/lib/gcc/<arch>-linux-<gnu>/13/"},
		{"/usr/lib/i386-linux-gnu/", "i386", "/usr/lib/<arch>-linux-<gnu>/"},
		{"/usr/lib/ruby/gems/3.3.0/extensions/x86-linux-gnu/3.3.0/", "i386", "/usr/lib/ruby/gems/3.3.0/extensions/<arch>-linux-<gnu>/3.3.0/"},
		{"/usr/lib/x86-linux-gnu/x", "amd64", "/usr/lib/x86-linux-gnu/x"},
		{"/usr/lib/jvm/java-21-openjdk-amd64/bin/java", "amd64", "/usr/lib/jvm/java-21-openjdk-<arch>/bin/java"},
		{"/usr/share/doc/gcc-14-base/README.Debian.armhf.gz", "armhf", "/usr/share/doc/gcc-14-base/README.Debian.<arch>.gz"},
		{"/usr/lib/s390x-linux-gnu/s390x/", "s390x", "/usr/lib/<arch>-linux-<gnu>/<arch>/"},
		{"/etc/apparmor.d/arm", "armhf", "/etc/apparmor.d/arm"},
		{"/usr/lib/i386-linux-gnu/x", "amd64", "/usr/lib/i386-linux-gnu/x"},
		{"/usr/share/amd64foo/amd64", "arm64", "/usr/share/amd64foo/amd64"},
		{"/usr/share/amd64foo/amd64", "amd64", "/usr/share/amd64foo/<arch>"},
		{"/usr/lib/x86_64-linux-gnu/x", "nope", "/usr/lib/x86_64-linux-gnu/x"},
	} {
		assert.Equal(t, Normalise(tc.path, tc.arch), tc.want, tc.path+" on "+tc.arch)
	}
}

func item(path, arch string) Item {
	return Item{Key: Normalise(path, arch), Raw: path}
}

func TestFoldSingletonShowsRaw(t *testing.T) {
	g := Fold([]string{"amd64", "arm64", "s390x"}, [][]Item{
		{item("/usr/lib/x86_64-linux-gnu/", "amd64")},
		{item("/usr/lib/aarch64-linux-gnu/", "arm64")},
		{item("/usr/lib/s390x-linux-gnu/ld64.so.1", "s390x")},
	})
	assert.Len(t, g, 2)
	assert.EqualArrays(t, g[0].Arches, []string{"amd64", "arm64"})
	assert.Len(t, g[0].Entries, 1)
	assert.Equal(t, g[0].Entries[0].Shown(), "/usr/lib/<arch>-linux-<gnu>/")
	assert.EqualArrays(t, g[0].Entries[0].Raw, []string{"/usr/lib/x86_64-linux-gnu/", "/usr/lib/aarch64-linux-gnu/"})
	assert.EqualArrays(t, g[1].Arches, []string{"s390x"})
	assert.Equal(t, g[1].Entries[0].Shown(), "/usr/lib/s390x-linux-gnu/ld64.so.1")
}

func TestFoldMostArchesFirst(t *testing.T) {
	plain := func(paths ...string) []Item {
		var out []Item
		for _, p := range paths {
			out = append(out, Item{Key: p, Raw: p})
		}
		return out
	}
	g := Fold([]string{"amd64", "arm64", "armhf", "i386"}, [][]Item{
		plain("/a", "/x"),
		plain("/a", "/y", "/x"),
		plain("/a", "/y", "/z"),
		plain("/y", "/z"),
	})
	assert.Len(t, g, 4)
	assert.EqualArrays(t, g[0].Arches, []string{"amd64", "arm64", "armhf"})
	assert.Equal(t, g[0].Entries[0].Key, "/a")
	assert.EqualArrays(t, g[1].Arches, []string{"arm64", "armhf", "i386"})
	assert.Equal(t, g[1].Entries[0].Key, "/y")
	assert.EqualArrays(t, g[2].Arches, []string{"amd64", "arm64"})
	assert.Equal(t, g[2].Entries[0].Key, "/x")
	assert.EqualArrays(t, g[3].Arches, []string{"armhf", "i386"})
	assert.Equal(t, g[3].Entries[0].Key, "/z")
}

func TestFoldSplitsOnTagAndSortsByKey(t *testing.T) {
	g := Fold([]string{"amd64", "arm64"}, [][]Item{
		{{Key: "/b", Tag: "elf", Raw: "/b"}, {Key: "/a", Tag: "text", Raw: "/a"}},
		{{Key: "/a", Tag: "text", Raw: "/a"}, {Key: "/b", Tag: "data", Raw: "/b"}},
	})
	assert.Len(t, g, 3)
	assert.EqualArrays(t, g[0].Arches, []string{"amd64", "arm64"})
	assert.Equal(t, g[0].Entries[0].Key, "/a")
	assert.Equal(t, g[0].Entries[0].Tag, "text")
	assert.EqualArrays(t, g[1].Arches, []string{"amd64"})
	assert.Equal(t, g[1].Entries[0].Tag, "elf")
	assert.EqualArrays(t, g[2].Arches, []string{"arm64"})
	assert.Equal(t, g[2].Entries[0].Tag, "data")
}
