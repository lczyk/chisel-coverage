# chisel-slice-coverage: design

a development tool for [chisel-releases](https://github.com/canonical/chisel-releases) work. for a package, it reports which paths of the package's `.deb` no slice in `slices/<package>.yaml` declares, per architecture, plus what each slice does grab. it is not a chisel feature and lives outside canonical's namespace.

this file is a working note for the first implementation. it goes away once the readme carries what a user needs; the reasoning stays in git history.

## principle

chisel does all the work. the tool never reads an archive, verifies a signature, parses a `Packages` index, extracts a `.deb` or matches a glob. every one of those is a `chisel cut` against a throwaway release, and the tool only walks the resulting rootfs directories and does set arithmetic. whatever chisel binary runs is the one whose semantics apply, which is the point: run it with the chisel under test.

the python prototype (`thething-2`) fetched debs itself and matched sdf globs with its own glob code. both are gone.

## how it works

for each package:

1. read `<release>/chisel.yaml` verbatim and `<release>/slices/<package>.yaml`.
2. synthesise one sdf holding, for the same package:
    - `cov-all` with a single entry `/**` -- the whole deb.
    - every real slice, stripped (see below). with `--slice`, only those.
    - one `cov-ignore-<n>` per `--ignore` glob, when any.

    globs in one package never conflict with each other or with explicit paths of that package, so all of this is one file.
3. write a throwaway release: the original `chisel.yaml` plus that one sdf, via `chisel-proxy/pkg/temp-release`.
4. per architecture, run `chisel cut --release <tmp> --root <dir> --arch <a> --ignore unmaintained --ignore unstable <package>_<slice>` once per slice in the synthesised sdf. walk each rootfs into a set of paths (files, symlinks and directories, directories with a trailing slash, root itself excluded), then delete the rootfs.
5. arithmetic per architecture:
    - `all` = the `cov-all` set
    - `covered` = union of the real slices' sets
    - `ignored` = (`all` - `covered`) intersected with the union of the `cov-ignore-<n>` sets
    - `uncovered` = `all` - `covered` - `ignored`

    a path claimed by two slices shows under both. ignores only ever shrink `uncovered`; a covered path that matches an ignore stays covered.

a slice with no entries left after stripping is not cut; its set is empty. chisel refuses a path or glob that matches nothing in the package (`no content at <path>`), so each ignore glob gets a slice of its own: a `no content` failure of an ignore slice is an empty set, while the same failure of a real slice is an error, since the real cut would fail the same way. the manifest is never used: a slice with `generate: manifest` is just a slice whose generated entry gets stripped like any other, and nothing depends on manifest generation working.

### stripping

the synthesised slices keep only what the deb can answer: the path keys, with their `arch:` lists. everything else goes.

- package- and slice-level `essential` -- other packages are not this deb.
- `mutate` and `until` -- a path removed after mutation is still declared, so it counts as covered.
- `text`, `symlink`, `make: true`, `generate` -- generated content, not deb content. a deb directory the sdf creates with `make: true` is reported uncovered; that is rare and the honest answer.
- `mode`, `mutable`, `prefer` -- attributes, no effect on which deb paths are claimed.
- `copy: <src>` -- the entry is re-keyed by `<src>` so the deb path gets the credit.

`archive:` is kept, so a pinned package is cut from the archive the sdf names.

### directories

everything in the `cov-all` rootfs counts, directories included. a directory is covered when it exists in a slice's rootfs, which includes the implicit parents chisel creates for a file. so `/usr/bin/` is covered by `/usr/bin/bash`, and an uncovered directory is one with no covered content at all, e.g. an empty `/etc/foo.d/` the deb ships and no slice names.

## scheduling

chisel's cache (`$XDG_CACHE_HOME/chisel`) is content-addressed and shared across runs; the tool uses it as-is. one wrinkle: when two chisel processes miss on the same blob at once, both write to the same `<digest>.tmp` and one of them fails its digest check (`internal/cache/cache.go`, `Create` with a known digest). concurrent cuts must therefore never share a cold blob.

- one lane per architecture. a lane runs its cuts one after another: for each package, `cov-all`, then each slice, then the ignore slices. nothing on one architecture ever races itself.
- the lead lane (the first architecture given; the host's by default) runs to completion alone, then the remaining lanes run in parallel. the only blobs lanes can share are `arch: all` debs, and the lead lane has fetched those by the time the others start.

on a warm cache a cut is well under a second, so the lead lane costs seconds per package. `-j` is not needed yet.

## chisel

- the binary is `$CHISEL`, else `chisel` on `PATH`. its full environment is passed through.
- stdout and stderr of every cut are captured. a failed cut prints its command line and output and the run exits 2; `-v` streams every cut's output as it happens, prefixed with the architecture and slice.
- `cannot find package "<package>" in archive` on the `cov-all` cut means the package is not built for that architecture. that is a result ("not available"), not an error, and the lane skips the package's other cuts.
- the version comes from chisel's `Fetching pool/.../<name>_<version>_<arch>.deb...` log line of the `cov-all` cut: the pool filename, so no epoch, plus the deb architecture (`all` or the real one). if the line is missing the version is unknown and nothing fails.
- `--ignore unmaintained --ignore unstable` is always passed, so an old or devel release still cuts. an older chisel without `--ignore` fails every cut with its own "unknown flag" message, which the tool surfaces unchanged.

## cli

```
chisel-slice-coverage [flags] <package>...
      --release=<dir>          chisel-releases checkout (default: .)
      --arch=<a>[,<a>...]      architectures to check; "all" for every one
                               (default: host)
      --slice=<s>[,<s>...]     only these slices, bare or <package>_<slice>
                               (default: all)
      --ignore=<glob>[,...]    leave matching paths out of the uncovered set
                               (chisel glob syntax)
      --json                   machine-readable output, one entry per
                               architecture
      --fail-uncovered         exit 1 when any architecture has uncovered paths
      --keep                   keep the temp releases and rootfs dirs
  -v, --verbose                stream chisel's output
      --version                print the version and exit
  -h, --help                   this text
```

flags are parsed by go-flags, the library chisel uses, so `--flag=value`, `--flag value` and flags after positionals behave as in chisel; `version` and `help` as the first argument are kept as `--version` / `--help` aliases. exit codes: 0 ran fine; 1 only with `--fail-uncovered`; 2 usage error, io error or a cut that failed. no colour, no default ignores.

## report

text, to stdout. architectures with identical results share a block; identical means equal version, availability, and equal `uncovered`, `ignored` and per-slice sets after normalising the multiarch triplet (`x86_64-linux-gnu` -> `<arch>-linux-<gnu>`; the cpu part when followed by `-linux-`, the abi part only as a directory component). errors and "not available" are never grouped with results.

```
bash 5.2.21-2ubuntu4 [amd64, arm64]
  total 42  covered 23  ignored 0  uncovered 19

  uncovered:
    /usr/share/man/man1/bash.1.gz
    ...

  slices:
    bins (4):
      /usr/bin/bash
      ...

bash [riscv64]: not available
```

`--json` is per architecture, raw paths, no grouping:

```json
{"packages": [{"name": "bash", "arches": [
  {"arch": "amd64", "available": true, "version": "5.2.21-2ubuntu4", "deb-arch": "amd64",
   "error": null, "total": 42, "covered": [...], "uncovered": [...], "ignored": [...],
   "slices": {"bins": [...], ...}}]}]}
```

## layout

mirrors chisel-proxy: `cmd/`, `internal/{cli,sdf,cut,coverage}`, `version.go` + `VERSION`, `e2e/`, lowercase `makefile`, agpl-3.

- `internal/sdf` -- read an sdf, strip, synthesise.
- `internal/cut` -- run one chisel cut, classify its failure, read the version line, walk a rootfs.
- `internal/coverage` -- lanes, set arithmetic, normalisation and grouping.
- `internal/cli` -- flags, text and json rendering, exit codes.

deps: `github.com/jessevdk/go-flags` (what chisel uses), `gopkg.in/yaml.v3`, `github.com/lczyk/chisel-proxy/pkg` (`temp-release`; `deb` and `apt` in tests), `github.com/lczyk/assert`, `github.com/lczyk/version/go`.

## testing

- unit tests with a fake `chisel` script that populates `--root` from the slice name. one of them takes a per-architecture lock in the fake and fails if two cuts on one architecture overlap.
- an integration test with the real `chisel` against an in-memory signed archive (`chisel-proxy/pkg/apt`) of handmade debs, reached through `http_proxy` and a throwaway `XDG_CACHE_HOME`. this pins the chisel assumptions above (globs coexisting in one sdf, implicit parents, the ignore slices, `no content`) offline. skipped when `chisel` is not found.
- `e2e/` against the real archive and a real chisel-releases clone, behind `-tags e2e` and `CHISEL_SLICE_COVERAGE_E2E=1`.

## carried over from the prototype

the prototype grew three speedups shortly before this rewrite. none needs code here:

- caching `Packages.gz` for six hours: chisel caches the decompressed index by the digest `InRelease` lists, which is a hit for as long as the index is unchanged. only `InRelease` itself (tens of kb per suite) is re-fetched per cut.
- fetching an `arch: all` deb once per run: chisel's cache is content-addressed, so the second architecture is a hit. the lead lane exists to make that true on a cold cache.
- extracting debs in-process instead of `dpkg-deb`: chisel extracts.

## later, maybe

- a `--real` mode cutting the slices against the untouched release, essentials and mutate included, for "what does a real cut produce" rather than "what does the sdf declare".
- `--release <name>` fetching a release the way chisel does.
- `-j` to cap lanes, once someone runs `--arch all` across many packages.
- the chisel cache race is a one-line upstream fix (`os.CreateTemp(dir, digest+".tmp.*")`); with it, the lead-lane wait can go.
