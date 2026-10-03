<p align="center">
  <img src="assets/banner.svg" alt="file-syncer-cmp" width="900"/>
</p>

[![CI](https://github.com/slmingol/file-syncer-cmp/actions/workflows/ci.yml/badge.svg)](https://github.com/slmingol/file-syncer-cmp/actions/workflows/ci.yml)
[![Release](https://github.com/slmingol/file-syncer-cmp/actions/workflows/release.yml/badge.svg)](https://github.com/slmingol/file-syncer-cmp/actions/workflows/release.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/slmingol/file-syncer-cmp)](https://goreportcard.com/report/github.com/slmingol/file-syncer-cmp)
![Go Version](https://img.shields.io/badge/go-1.21+-00ADD8?logo=go)
![Platforms](https://img.shields.io/badge/platform-linux%20%7C%20darwin-lightgrey)
![Arch](https://img.shields.io/badge/arch-amd64%20%7C%20arm64-blue)

Compare media files between two locations — even when the directory structure has been reorganized. Designed for auditing rsync'd content from a Transmission server to a NAS.

**Matches by filename + size, not path.** A file moved from `artist/album/track.flac` to `music/various/track.flac` is still considered synced.

---

## Install

Download the binary for your platform from [Releases](https://github.com/slmingol/file-syncer-cmp/releases):

| Platform | Binary |
|---|---|
| Linux x86_64 | `file-syncer-cmp-linux-amd64` |
| Linux ARM64 (NAS, Pi) | `file-syncer-cmp-linux-arm64` |
| macOS Intel | `file-syncer-cmp-darwin-amd64` |
| macOS Apple Silicon | `file-syncer-cmp-darwin-arm64` |

```bash
chmod +x file-syncer-cmp-linux-arm64
mv file-syncer-cmp-linux-arm64 /usr/local/bin/file-syncer-cmp
```

Or build from source (requires Go 1.21+):

```bash
go install github.com/slmingol/file-syncer-cmp@latest
```

---

## Usage

### Two-phase (recommended — server and NAS not mounted at the same time)

```bash
# On the Transmission server, for each disk:
file-syncer-cmp scan /mnt/disk1 --output disk1.json
file-syncer-cmp scan /mnt/disk2 --output disk2.json

# Scan the NAS once:
file-syncer-cmp scan /volume1/media --output nas.json

# Copy *.json files to a single machine, then compare:
file-syncer-cmp compare disk1.json nas.json
file-syncer-cmp compare disk2.json nas.json

# HTML report:
file-syncer-cmp compare disk1.json nas.json --format html > report.html
```

### One-shot (both paths accessible at once)

```bash
file-syncer-cmp sync-check /mnt/disk1 /volume1/media
file-syncer-cmp sync-check /mnt/disk1 /volume1/media --format html > report.html
```

### With hash verification

Adds a partial content hash (first + last 512 KB) to catch corruption:

```bash
file-syncer-cmp scan /mnt/disk1 --output disk1.json --hash
file-syncer-cmp scan /volume1/media --output nas.json --hash
file-syncer-cmp compare disk1.json nas.json --hash
```

---

## Output

Three categories of findings:

| Category | Meaning |
|---|---|
| **Missing** | File exists in source, not found anywhere in dest by name |
| **Size mismatch** | Name found in dest but byte count differs (possible re-encode) |
| **Hash mismatch** | Same name + size, different content — requires `--hash` |

Text output (default):

```
FILE SYNC REPORT
--------------------------------------------------------------------------------
Source:  /mnt/disk1 (scanned 2026-10-03 14:22, 1842 files)
Dest:    /volume1/media (scanned 2026-10-03 14:23, 1809 files)
--------------------------------------------------------------------------------

MISSING FILES (3) - present in source, not found in dest:
  [MISSING] artist/album/track03.flac  (48.2 MiB)
  [MISSING] movies/film.mkv  (12.4 GiB)
  [MISSING] podcasts/ep42.mp3  (31.1 MiB)

SIZE MISMATCH (1) - name found in dest but different size:
  [MISMATCH] concert.mkv
    src: concerts/2023/concert.mkv  (8.1 GiB)
    dst: video/concert.mkv  (7.9 GiB)

SUMMARY: 3 missing, 1 size-mismatch, 0 hash-mismatch
```

Pass `--format html` for a styled report, `--format json` for machine-readable output.

---

## Flags

### `scan`

| Flag | Default | Description |
|---|---|---|
| `--output` | stdout | Write index to file instead of stdout |
| `--ext` | media files | Comma-separated extensions to include, e.g. `mp3,flac,mkv` |
| `--hash` | off | Compute partial hash of each file |
| `--workers` | 8 | Parallel scan workers |

Default extensions: `mp3 flac wav aac ogg opus m4a wma alac aiff mkv mp4 avi mov m4v ts m2ts wmv webm vob iso nfo srt ass sub`

### `compare`

| Flag | Default | Description |
|---|---|---|
| `--format` | `text` | Output format: `text`, `json`, `html` |
| `--hash` | off | Compare hashes (both indexes must include hashes) |

### `sync-check`

Accepts all flags from both `scan` and `compare`.

---

## License

MIT
