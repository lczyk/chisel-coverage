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
		{"/usr/lib/x86-linux-gnu/x", "amd64", "/usr/lib/<arch>-linux-<gnu>/x"},
		{"/usr/lib/jvm/java-21-openjdk-amd64/bin/java", "amd64", "/usr/lib/jvm/java-21-openjdk-<arch>/bin/java"},
		{"/usr/share/doc/gcc-14-base/README.Debian.armhf.gz", "armhf", "/usr/share/doc/gcc-14-base/README.Debian.<arch>.gz"},
		{"/usr/lib/s390x-linux-gnu/s390x/", "s390x", "/usr/lib/<arch>-linux-<gnu>/<arch>/"},
		// A word of the architecture anywhere, in every spelling an
		// ecosystem uses; a word of another architecture is left alone.
		{"/etc/apparmor.d/arm", "armhf", "/etc/apparmor.d/<arch>"},
		{"/etc/apparmor.d/apparmor", "armhf", "/etc/apparmor.d/apparmor"},
		{"/usr/lib/jvm/java-8-openjdk-arm64/jre/lib/aarch64/jli/libjli.so", "arm64", "/usr/lib/jvm/java-8-openjdk-<arch>/jre/lib/<arch>/jli/libjli.so"},
		{"/usr/lib/jvm/java-8-openjdk-ppc64el/jre/lib/ppc64le/libjava.so", "ppc64el", "/usr/lib/jvm/java-8-openjdk-<arch>/jre/lib/<arch>/libjava.so"},
		{"/usr/lib/go-1.26/pkg/tool/linux_386/asm", "i386", "/usr/lib/go-1.26/pkg/tool/linux_<arch>/asm"},
		{"/usr/lib/go-1.26/pkg/tool/linux_arm/asm", "armhf", "/usr/lib/go-1.26/pkg/tool/linux_<arch>/asm"},
		{"/usr/lib/i386-linux-gnu/libunwind-x86.so.8", "i386", "/usr/lib/<arch>-linux-<gnu>/libunwind-<arch>.so.8"},
		{"/usr/lib/x86_64-linux-gnu/libunwind-x86_64.so.8", "amd64", "/usr/lib/<arch>-linux-<gnu>/libunwind-<arch>.so.8"},
		{"/usr/lib/dotnet/packs/runtime.ubuntu.26.04-x64.Microsoft.DotNet.ILCompiler", "amd64", "/usr/lib/dotnet/packs/runtime.ubuntu.26.04-<arch>.Microsoft.DotNet.ILCompiler"},
		{"/usr/src/rustc/src/ci/docker/host-x86_64/dist-armhf-linux", "i386", "/usr/src/rustc/src/ci/docker/host-x86_64/dist-armhf-linux"},
		{"/usr/src/rustc/src/ci/docker/host-x86_64/dist-armhf-linux", "armhf", "/usr/src/rustc/src/ci/docker/host-x86_64/dist-<arch>-linux"},
		{"/usr/share/go-1.26/src/cmd/compile/internal/arm64/ssa.go", "armhf", "/usr/share/go-1.26/src/cmd/compile/internal/arm64/ssa.go"},
		// Debian's cross toolchain packages spell amd64 as x86-64, which is
		// not i386's x86 followed by -64.
		{"/usr/share/doc/binutils-x86-64-linux-gnu/copyright", "amd64", "/usr/share/doc/binutils-<arch>-linux-<gnu>/copyright"},
		{"/usr/share/doc/binutils-x86-64-linux-gnu/copyright", "i386", "/usr/share/doc/binutils-x86-64-linux-gnu/copyright"},
		// The kernel headers' arch directory is x86 on amd64 and i386 alike.
		{"/usr/src/linux-headers-7.0.0-42-generic/arch/x86/include/asm/io.h", "amd64", "/usr/src/linux-headers-7.0.0-42-generic/arch/<arch>/include/asm/io.h"},
		{"/usr/src/linux-headers-7.0.0-42-generic/arch/x86/include/asm/io.h", "i386", "/usr/src/linux-headers-7.0.0-42-generic/arch/<arch>/include/asm/io.h"},
		{"/usr/src/linux-headers-7.0.0-42-generic/arch/riscv/include/asm/io.h", "riscv64", "/usr/src/linux-headers-7.0.0-42-generic/arch/<arch>/include/asm/io.h"},
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

// A path that is the same on every architecture folds however its key came
// out, and is shown as it is.
func TestFoldOnRawToo(t *testing.T) {
	g := Fold([]string{"amd64", "arm64", "armhf"}, [][]Item{
		{item("/etc/apparmor.d/arm", "amd64"), item("/usr/bin/aarch64-linux-gnu-gcc", "amd64")},
		{item("/etc/apparmor.d/arm", "arm64"), item("/usr/bin/aarch64-linux-gnu-gcc", "arm64")},
		{item("/etc/apparmor.d/arm", "armhf"), item("/usr/bin/arm-linux-gnueabihf-gcc", "armhf")},
	})
	assert.Len(t, g, 1)
	assert.EqualArrays(t, g[0].Arches, []string{"amd64", "arm64", "armhf"})
	assert.Len(t, g[0].Entries, 2)
	assert.Equal(t, g[0].Entries[0].Shown(), "/etc/apparmor.d/arm")
	// amd64 and arm64 share the raw path, arm64 and armhf the key.
	assert.Equal(t, g[0].Entries[1].Shown(), "/usr/bin/<arch>-linux-<gnu>-gcc")
	assert.EqualArrays(t, g[0].Entries[1].Raw, []string{"/usr/bin/aarch64-linux-gnu-gcc", "/usr/bin/aarch64-linux-gnu-gcc", "/usr/bin/arm-linux-gnueabihf-gcc"})
}

// When folding on raw forms would put two items of one architecture in
// one entry, the keys alone decide, and where a key still holds one
// architecture twice (the x32 abi folds onto gnu), the k-th item of every
// architecture makes the k-th entry.
func TestFoldTwiceOnOneArch(t *testing.T) {
	g := Fold([]string{"amd64", "arm64"}, [][]Item{
		{item("/x/amd64/f", "amd64"), item("/x/aarch64/f", "amd64")},
		{item("/x/aarch64/f", "arm64")},
	})
	assert.Len(t, g, 2)
	assert.EqualArrays(t, g[0].Arches, []string{"amd64", "arm64"})
	assert.Equal(t, g[0].Entries[0].Shown(), "/x/<arch>/f")
	assert.EqualArrays(t, g[1].Arches, []string{"amd64"})
	assert.Equal(t, g[1].Entries[0].Shown(), "/x/aarch64/f")

	g = Fold([]string{"amd64", "arm64"}, [][]Item{
		{item("/usr/lib/x86_64-linux-gnu/x", "amd64"), item("/usr/lib/x86_64-linux-gnux32/x", "amd64")},
		{item("/usr/lib/aarch64-linux-gnu/x", "arm64")},
	})
	assert.Len(t, g, 2)
	for _, gr := range g {
		for _, e := range gr.Entries {
			assert.Len(t, e.Raw, len(gr.Arches))
		}
	}
	assert.EqualArrays(t, g[0].Arches, []string{"amd64", "arm64"})
	assert.EqualArrays(t, g[0].Entries[0].Raw, []string{"/usr/lib/x86_64-linux-gnu/x", "/usr/lib/aarch64-linux-gnu/x"})
	assert.EqualArrays(t, g[1].Arches, []string{"amd64"})
	assert.Equal(t, g[1].Entries[0].Shown(), "/usr/lib/x86_64-linux-gnux32/x")
}

// Tags fold on their raw forms too: a symlink whose target names the
// architecture on one of them and is the same everywhere.
func TestFoldOnRawTagToo(t *testing.T) {
	g := Fold([]string{"amd64", "arm64"}, [][]Item{
		{{Key: "/usr/bin/aarch64-linux-gnu-ld", Raw: "/usr/bin/aarch64-linux-gnu-ld", Tag: "symbolic link to aarch64-linux-gnu-ld.bfd", RawTag: "symbolic link to aarch64-linux-gnu-ld.bfd"}},
		{{Key: "/usr/bin/<arch>-linux-<gnu>-ld", Raw: "/usr/bin/aarch64-linux-gnu-ld", Tag: "symbolic link to <arch>-linux-<gnu>-ld.bfd", RawTag: "symbolic link to aarch64-linux-gnu-ld.bfd"}},
	})
	assert.Len(t, g, 1)
	assert.EqualArrays(t, g[0].Arches, []string{"amd64", "arm64"})
	assert.Equal(t, g[0].Entries[0].Shown(), "/usr/bin/aarch64-linux-gnu-ld")
	assert.Equal(t, g[0].Entries[0].Tag, "symbolic link to <arch>-linux-<gnu>-ld.bfd")
	// Different raw tags with no normalised form in common stay apart.
	g = Fold([]string{"amd64", "arm64"}, [][]Item{
		{{Key: "/a", Raw: "/a", Tag: "symbolic link to ld.1.gz"}},
		{{Key: "/a", Raw: "/a", Tag: "symbolic link to <arch>-linux-<gnu>-ld.bfd.1.gz", RawTag: "symbolic link to aarch64-linux-gnu-ld.bfd.1.gz"}},
	})
	assert.Len(t, g, 2)
}

// Entries are sorted by what is shown, which is the raw form when the
// architectures agree on it, not by the key.
func TestFoldSortsByShown(t *testing.T) {
	g := Fold([]string{"amd64", "armhf"}, [][]Item{
		{item("/lib/arm", "amd64"), item("/lib/apple", "amd64")},
		{item("/lib/arm", "armhf"), item("/lib/apple", "armhf")},
	})
	assert.Len(t, g, 1)
	assert.Equal(t, g[0].Entries[0].Shown(), "/lib/apple")
	assert.Equal(t, g[0].Entries[1].Shown(), "/lib/arm")
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
