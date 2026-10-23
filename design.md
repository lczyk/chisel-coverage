# chisel-slice-coverage: design

a development tool for [chisel-releases](https://github.com/canonical/chisel-releases) work. for a package, it reports which paths of the package's `.deb` no slice in `slices/<package>.yaml` declares, per architecture, plus what each slice does grab. it is not a chisel feature and lives outside canonical's namespace.

this file is a working note for the first implementation. it goes away once the readme carries what a user needs; the reasoning stays in git history.

## principle

chisel does all the work. the tool never reads an archive, verifies a signature, parses a `Packages` index, extracts a `.deb` or matches a glob. every one of those is a `chisel cut` against a throwaway release, and the tool only walks the resulting rootfs directories and does set arithmetic. whatever chisel binary runs is the one whose semantics apply, which is the point: run it with the chisel under test.

the python prototype (`thething-2`) fetched debs itself and matched sdf globs with its own glob code. both are gone.

## how it works

for each package:

1. read `<release>/chisel.yaml` verbatim and `<release>/slices/<package>.yaml`. `--release` is a checkout, or a release name like `ubuntu-24.04`: then the branch tarball is fetched from github the way chisel fetches it, into `~/.cache/chisel-slice-coverage/releases/<name>`, refreshed by etag on every run and served from the cache when github is unreachable.
2. synthesise one sdf holding, for the same package:
    - `cov-all` with a single entry `/**` -- the whole deb.
    - every real slice, stripped (see below). with `--slice`, only those.
    - one `cov-ignore-<n>` per `--ignore` glob, when any.
    - `cov-probe`, empty: its cut only says whether the package exists for an architecture.

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

chisel's cache (`$XDG_CACHE_HOME/chisel`) is content-addressed and shared across runs. one wrinkle: when two chisel processes miss on the same blob at once, both write to the same fixed `<digest>.tmp`, one of them loses its rename and the other may read a torn file (`internal/cache/cache.go`, `Create` with a known digest; canonical/chisel#340). concurrent cuts must therefore never share a cache they can both write to.

- the work is one unit per (package, architecture). `-j n` runs `n` cuts at once, `-1` (the default) one per cpu, `1` one after another. a pool of `n` workers pulls units in order; each worker cuts a unit's slices one after another (`cov-all`, then each slice, then the ignore slices).
- an `arch: all` package (the lead cut fetched `<name>_<version>_all.deb`) is cut in full on the lead architecture only; on every other architecture `cov-probe` is cut instead, a second rather than a full extract, and the lead result stands in where the package exists (i386's partial archive, say, may lack it). if chisel's log line ever stops naming the deb, nothing is detected as `all` and every architecture is cut in full as before.
- a cut's rootfs is removed in the background; a big one is thousands of unlinks the worker need not wait for.
- sigint or sigterm stops the run: no new cut starts, running chisels get sigint, the temp releases, rootfs dirs and cache views are removed, and the exit code is 130. a second signal ends the process at once, leaving whatever is left for the 24h sweep.
- an `arch: all` package's architectures fold on their real paths, not normalised ones: it is one deb everywhere, and a go source tree names every architecture somewhere, so normalising would keep the architectures apart.
- the lead architecture (the first given; the host's by default) runs to completion alone, then the rest. the only blobs two units can share are `arch: all` debs and the suite indexes, and the lead phase has fetched those by the time the others start.
- when more than one worker runs, every worker but the first gets a private view of the cache (`internal/cache`): a directory under `$XDG_CACHE_HOME/chisel-slice-coverage/` hard-linked to every blob of the shared cache, which chisel runs against through `XDG_CACHE_HOME`. a miss lands in the view only; after each package the new blobs are linked back into the shared cache, skipped when another worker got there first. views are reused across the two phases, removed at the end, and a view a crashed run left behind is swept after a day. seeding a view is one link per blob, a couple of seconds for a 15k-blob cache over a few goroutines. views are seeded one at a time: the links serialise on the directory in the kernel anyway, so three at once take three times as long each, and one at a time gets the first worker going sooner. at the end the views are removed all at once, each being thousands of unlinks. the first worker of a phase shares the cache as is: a view's blobs only ever enter it as complete files linked into place, so nothing trips the chisel running beside them. a view of a big cache costs about as much as cutting a small package, so a phase gets no more workers than its work pays for: the lead phase measures each package's cut time, and the second phase takes one worker per view's worth of that work (plus the free first one), capped at `-j`. if the view cannot be made (another filesystem, no write access) the worker shares the cache and says so on stderr.

on a warm cache a cut is well under a second; on a flaky archive a stalled connection holds one worker, not the run.

## chisel

- the binary is `$CHISEL`, else `chisel` on `PATH`. its full environment is passed through.
- stdout and stderr of every cut are captured. a failed cut prints its command line and output and the run exits 2. `-v` logs one line per finished cut on stderr (`26-10-22T09:34:09 arm64 ucf_cov-all 32`; `n/a` for a package missing on the arch, `0` for a glob matching nothing, `error`), as do the `--keep` notices. `-v` also reports each cache view as it is seeded, shaped like a cut line under the arch and package of the first cut waiting on it (`ppc64el golang-1.26-src_cov-cache-view 14935`: blobs linked) and ends with a summary (`98 cuts 1386 paths 33.5mb 19.2s`: cuts run, paths listed, bytes of regular files extracted, wall time); `-vv` also streams every cut's output as it happens, with chisel's own timestamp dropped. every logged line is `<yy-mm-ddThh:mm:ss> <arch> <slice> <text>`, space-separated, with `-` for an arch or slice the line has none of, so `cut -d' '` works on all of them.
- `cannot find package "<package>" in archive` on the `cov-all` or `cov-probe` cut means the package is not built for that architecture. that is a result ("not available"), not an error, and the worker skips the package's other cuts.
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
      --no-group               one block per architecture with its real paths,
                               instead of folding architectures with the same
                               result
      --json                   machine-readable output, one entry per
                               architecture
      --fail-uncovered         exit 1 when any architecture has uncovered paths
      --keep                   keep the temp releases and rootfs dirs
  -j, --jobs=<n>               cuts to run at once; -1 for one per cpu
                               (default: -1)
  -v, --verbose                one line per cut on stderr; twice also streams
                               chisel's output
      --version                print the version and exit
  -h, --help                   this text
```

flags are parsed by go-flags, the library chisel uses, so `--flag=value`, `--flag value` and flags after positionals behave as in chisel; `version` and `help` as the first argument are kept as `--version` / `--help` aliases. exit codes: 0 ran fine; 1 only with `--fail-uncovered`; 2 usage error, io error or a cut that failed. no default ignores. the text report gets a little colour (bold blue name, dim arches, counts green when as hoped -- covered non-zero, ignored and uncovered zero -- else yellow, red for uncovered; yellow "not available", red "error") only when stdout is a terminal and `NO_COLOR` is unset or empty, per no-color.org; piped output is plain.

## report

text, to stdout. architectures with identical results share a block; identical means equal version, availability, and equal `uncovered`, `ignored` and per-slice sets after normalising what names the architecture: the multiarch triplet becomes `<arch>-linux-<gnu>` wherever it is (the cpu part only where `-linux-` follows, so `apparmor` is safe; `i686` counts for i386; the abi part in file names too, so `perl5.40-arm-linux-gnueabihf.1.gz` folds), and the architecture's own name becomes `<arch>` where it stands as a word (`java-21-openjdk-amd64`, `README.Debian.armhf.gz`). a sweep of every 26.04 package showed no path that folds onto another on the same architecture, so folding can only split, never merge wrongly. errors and "not available" are never grouped with results. a group of one architecture shows its real paths, not the folded ones, and `--no-group` makes every architecture such a group.

```
bash 5.2.21-2ubuntu4 [amd64, arm64]
total 42 covered 23 ignored 0 uncovered 19
/usr/share/man/man1/bash.1.gz
...
bash [riscv64]: not available
```

the text report is what is uncovered and nothing else: no indentation, no blank lines, so it pipes into other tools; what each slice takes is only in the json. `--json` is per architecture, raw paths, no grouping:

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
- `internal/cache` -- private views of chisel's cache for concurrent cuts.
- `internal/release` -- `--release` as a checkout or a release name fetched from github and cached.
- `internal/coverage` -- the worker pool, set arithmetic, normalisation and grouping.
- `internal/cli` -- flags, text and json rendering, exit codes.

deps: `github.com/jessevdk/go-flags` (what chisel uses), `gopkg.in/yaml.v3`, `github.com/lczyk/chisel-proxy/pkg` (`temp-release`; `deb` and `apt` in tests), `github.com/lczyk/assert`, `github.com/lczyk/version/go`.

## testing

- unit tests with a fake `chisel` script that populates `--root` from the slice name. one of them takes a per-architecture lock in the fake and fails if two cuts overlap with `-j 1`; another has the fake behave like chisel's cache and checks every cut ran in a view and every blob came back.
- an integration test with the real `chisel` against an in-memory signed archive (`chisel-proxy/pkg/apt`) of handmade debs, reached through `http_proxy` and a throwaway `XDG_CACHE_HOME`. this pins the chisel assumptions above (globs coexisting in one sdf, implicit parents, the ignore slices, `no content`) offline. skipped when `chisel` is not found.
- `e2e/` against the real archive and a real chisel-releases clone, behind `-tags e2e` and `CHISEL_SLICE_COVERAGE_E2E=1`.

## carried over from the prototype

the prototype grew three speedups shortly before this rewrite. none needs code here:

- caching `Packages.gz` for six hours: chisel caches the decompressed index by the digest `InRelease` lists, which is a hit for as long as the index is unchanged. only `InRelease` itself (tens of kb per suite) is re-fetched per cut.
- fetching an `arch: all` deb once per run: chisel's cache is content-addressed, so the second architecture is a hit. the lead phase exists to make that true on a cold cache.
- extracting debs in-process instead of `dpkg-deb`: chisel extracts.

## later, maybe

- a `--real` mode cutting the slices against the untouched release, essentials and mutate included, for "what does a real cut produce" rather than "what does the sdf declare".
- once canonical/chisel#340 lands, views are only a bandwidth saver and the lead phase can go.
