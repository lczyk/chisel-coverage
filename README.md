# chisel-coverage

[![Ubuntu](https://img.shields.io/badge/Ubuntu-E95420?logo=ubuntu&logoColor=white)](https://ubuntu.com)
[![chisel](https://img.shields.io/badge/%F0%9F%94%A8-chisel-E95420)](https://github.com/canonical/chisel)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/lczyk/chisel-coverage)
![GitHub Tag](https://img.shields.io/github/v/tag/lczyk/chisel-coverage?filter=v*&sort=semver&label=release)

development tool for [chisel-releases](https://github.com/canonical/chisel-releases) work. for a package, it reports which paths of the `.deb` its slice definition file leaves out, per architecture.

## why

when writing or reviewing a slice definition we want to know what we might have left out. this is a pain to do by hand, especially on every arch.

## use

```shell
chisel-coverage --verbose --release ubuntu-26.04 --arch all \
  --ignore '/usr/share/**' \
  --ignore '/usr/lib/jvm/java-17-openjdk-*/man/**' \
  openjdk-17-jdk-headless
26-10-23T14:19:11 amd64 openjdk-17-jdk-headless_cov-cache-view 14976
26-10-23T14:19:12 amd64 openjdk-17-jdk-headless_cov-all 139
26-10-23T14:19:12 amd64 openjdk-17-jdk-headless_core 12
26-10-23T14:19:12 amd64 openjdk-17-jdk-headless_standard 19
...
26-10-23T14:20:51 63 cuts 2089 paths 1090.1mb 18.1s
openjdk-17-jdk-headless 17.0.20.1+1-1~26.04 [amd64, arm64, armhf, i386, ppc64el, riscv64, s390x]
total 133 covered 105 ignored 26 uncovered 2
/usr/lib/jvm/java-17-openjdk-<arch>/lib/
/usr/lib/jvm/java-17-openjdk-<arch>/lib/src.zip
openjdk-17-jdk-headless 17.0.20.1+1-1~26.04 [amd64, arm64, armhf, i386, ppc64el, riscv64]
total 2 covered 1 ignored 1 uncovered 0
openjdk-17-jdk-headless 17.0.20.1+1-1~26.04 [amd64, arm64, ppc64el]
total 1 covered 1 ignored 0 uncovered 0
openjdk-17-jdk-headless 17.0.20.1+1-1~26.04 [armhf, i386, riscv64]
total 1 covered 0 ignored 0 uncovered 1
/usr/lib/jvm/java-17-openjdk-<arch>/jmods/jdk.hotspot.agent.jmod
openjdk-17-jdk-headless 17.0.20.1+1-1~26.04 [amd64, arm64]
total 3 covered 3 ignored 0 uncovered 0
```

every entry of the deb counts, directories included. a directory is covered once anything under it is. `--arch all` checks every architecture, and architectures with the same version are folded on what their debs ship. `<arch>` and `<gnu>` in paths are normalised and paths are grouped across arches. `--no-group` ungroups. `--json` never groups. 

several packages can be given at once, and all cuts run in parallel. jobs get work at slice-level granularity. one job per cpu by default (`--jobs=-1`) and each job is set up with a private view of the cache so concurrent cuts dont clobber one-anothe's caches. `chisel` binary is found on `PATH`, or set `CHISEL=/path/to/chisel`.

you can add paths to `--ignore`. the syntax is identical to chisel's SDFs since that's what ignore is doing under the hood. in fact, this entire tool is written using pattented (not really) [`chisel-proxy`](https://github.com/lczyk/chisel-proxy) technology. `--ignore` only ever shrinks the uncovered set so a covered path that matches an ignore stays covered.

## how it works ?

chisel does the work. for each package the tool writes a throwaway release holding `chisel.yaml` and a single slice definition (that's the `chisel-proxy` tech). the package's own slices are tehn stipped of from essentials, mutate scripts, `text` / `symlink` / `make` / `generate` entries etc, and `copy` entries are keyed by their source. extra slice (`cov-all`) with `/**` is also added to get the content of the entie deb, to compare. ignore globs are also extra slices. every slice is then `cut` to get the content of its rootffs.

so archive selection, suites, priorities, pro archives, signatures, `arch:` lists and glob semantics are all whatever the particular chisel binary does. the cache is chisel's own too. 

## build

```shell
make build      # ./bin/chisel-coverage
make test       # unit tests; the real-chisel ones skip without `chisel`
make e2e        # real chisel against the real archive and a chisel-releases clone (needs network)
```
