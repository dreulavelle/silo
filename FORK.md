# This is a fork

Upstream is [Silo-Server/silo-server](https://github.com/Silo-Server/silo-server) (AGPL-3.0).
This fork exists to take Silo somewhere upstream has declined to go: **on-demand
playback**, where a library item is created instantly as a placeholder and its
stream is resolved at the moment someone presses play.

Upstream was asked for `.strm` support and declined. That is their call to make,
and this fork is not a criticism of it — it is a different product direction on
a shared base.

## The one rule

**Upstream owns everything except the seams we declare.**

Every line of fork logic lives in a fork-owned path. Upstream files get the
smallest possible edit — ideally a map entry, a top-of-function guard, or a
single call site — and every one of them is declared in
[`fork/touched-files.txt`](fork/touched-files.txt).

This is enforced by CI (`fork-ci` → *fork invariants*), not by good intentions.

Fork-owned paths:

| Path | Purpose |
|---|---|
| `internal/strm/` | All on-demand playback logic |
| `migrations/sql/9*.sql` | Fork schema (see *Migrations* below) |
| `fork/` | Pin, manifest, fork docs |
| `scripts/fork-*.sh` | Fork tooling |
| `.github/workflows/fork-*.yml` | Fork CI |

The reason is arithmetic. Rebase pain scales with how often upstream edits the
same *hunks* we do, not with how much code we add. Ten thousand lines in
`internal/strm/` cost nothing at rebase time. Ten lines scattered through
`processFile` cost an afternoon a month, forever.

## Upstream pinning

Upstream publishes Docker images, not git tags, so there is no release train to
follow and `main` moves daily. We therefore pin an explicit commit in
[`fork/upstream.pin`](fork/upstream.pin) and advance it deliberately.

```sh
./scripts/fork-sync.sh check     # read-only: drift + predicted conflicts
./scripts/fork-sync.sh rebase    # trial rebase in a scratch worktree, build, test
./scripts/fork-sync.sh advance   # do it for real, rewrite the pin, commit
```

`check` and `rebase` never touch your working tree or move a branch. Only
`advance` does, and it refuses to run on a dirty tree.

CI runs `rebase` nightly and files a single `fork-sync`-labelled issue when the
answer is "not safe yet", commenting on it each subsequent night rather than
opening a new one. It closes the issue automatically once the fork is clean
again. The point is to find out about a breaking upstream change within a day,
while the delta is still a handful of commits.

### Conflict prediction

Before attempting anything, `fork-sync.sh` cross-references the files upstream
touched since the pin against `fork/touched-files.txt`. An overlap is not a
guaranteed conflict, but it is free early warning and it tells you exactly which
seam to look at first.

## Migrations

Fork migrations use a `9` prefix — `migrations/sql/9<13-digits>_name.sql` — so
they always sort after any plausible upstream timestamp (upstream uses
`20260709233718_...`, i.e. `2`-prefixed).

This matters more than it looks. Goose orders by filename. If a fork migration
sorted *between* two upstream migrations, then every rebase that pulled in a new
upstream migration would silently reorder our schema changes relative to theirs
— and any fork migration that altered a table an interleaved upstream migration
also altered would break in ways that only show up on a fresh database.

**Fork migrations never `ALTER` an upstream table.** If one is ever needed, it
creates a fork-owned table keyed by the upstream primary key instead:

```sql
CREATE TABLE strm_sources (
    media_file_id integer PRIMARY KEY
        REFERENCES media_files(id) ON DELETE CASCADE,
    ...
);
```

A foreign key is a far weaker coupling than a column. Upstream can add, rename,
and reorder `media_files` columns all day without touching us.

### There are currently no fork migrations, and that is the goal

On-demand playback needs no schema change in Silo. Everything the server has to
know is derivable from the file path — `.strm` means placeholder — so the
scanner and playback branch on `strm.IsPlaceholderPath` and nothing else. All
resolution state (pins, cached targets, request history, failure counts) lives
in the resolver plugin's own schema, which upstream never sees and never
migrates.

Keeping it that way is worth real effort. Zero fork migrations means zero
migration ordering conflicts, forever, which removes the single nastiest class
of long-lived-fork breakage: a schema divergence that only manifests on a fresh
database, long after the rebase that caused it.

## Upstream workflows

The upstream workflows in `.github/workflows/` (`claude.yml`,
`discord-commits.yml`, `docker.yml`, `v1-proposal-labeler.yml`) are **not**
edited or deleted here — deleting a file upstream keeps editing is a recurring
conflict for no benefit.

Instead, **disable them in the GitHub UI** for this repository:
*Actions → select workflow → ⋯ → Disable workflow*.

Do this before the first push. `discord-commits.yml` in particular will post
this fork's commits to upstream's Discord if left enabled.

## What this fork changes

See [`fork/touched-files.txt`](fork/touched-files.txt) for the authoritative
list. Narratively:

1. **`.strm` becomes a recognized media extension.** One entry in the scanner's
   `videoExtensions` map.
2. **`.strm` files are not probed at scan time.** They have no local bytes to
   probe. Metadata is populated once, out of band, by whatever wrote the
   placeholder.
3. **Remote rows are exempt from probe repair.** Upstream's
   `needsCriticalProbeRepairScanState` flags any row with `duration <= 0` or
   empty codecs for re-probe on every scan. Without an exemption a placeholder
   library would re-probe itself forever — a self-inflicted denial of service
   against our own resolver. This has no Jellyfin analogue; it is specific to
   Silo's repair layer.
4. **Playback resolves and redirects.** A media file with a `strm_sources` row
   is answered with a `302` to a freshly resolved URL instead of `os.Open`.

### Why 302 and never 301

FFmpeg caches `301` and `308` redirects for the life of the `HTTPContext` and
does not cache `302`/`303`/`307` (`libavformat/http.c`). A cached redirect to an
expired debrid link is unrecoverable without restarting playback. On a seek that
needs a new connection, FFmpeg reverts to the *original* URI — so with `302` a
seek re-resolves through us, which is exactly the behaviour we want.

### Scheme validation is not optional

`.strm` targets are restricted to `http`, `https`, `rtsp`, and `rtp`.

Jellyfin shipped `.strm` without this and it became the arbitrary-file-read
primitive in an RCE chain — [CVE-2026-35031 / GHSA-j2hf-x4q5-47j3](https://github.com/jellyfin/jellyfin/security/advisories/GHSA-j2hf-x4q5-47j3),
CVSS 9.9. A `.strm` containing `/etc/passwd` or `file:///...` must never resolve.
We are adopting the fix before shipping the bug.

## Getting a build

```sh
make build          # frontend + go binary
go test ./...       # needs web/dist to exist; see below
```

The Go build embeds `web/dist`, so `go build`/`go test` fail on a clean checkout
until the frontend is built. For Go-only work, stub it:

```sh
mkdir -p web/dist && echo '<!doctype html>' > web/dist/index.html
```

`web/dist/` is gitignored, so the stub is invisible to git. CI does exactly this.
