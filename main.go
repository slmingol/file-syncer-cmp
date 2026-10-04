package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

var version = "dev"

var defaultExts = []string{
	"mp3", "flac", "wav", "aac", "ogg", "opus", "m4a", "wma", "alac", "aiff",
	"mkv", "mp4", "avi", "mov", "m4v", "ts", "m2ts", "wmv", "webm", "vob",
	"iso", "nfo", "srt", "ass", "sub",
}

type FileRecord struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Ext         string `json:"ext"`
	Modified    int64  `json:"modified"`
	Hash        uint64 `json:"hash,omitempty"`
	SourceIndex string `json:"-"` // which index this came from (not serialized)
}

type Index struct {
	Root      string       `json:"root"`
	ScannedAt time.Time    `json:"scanned_at"`
	Files     []FileRecord `json:"files"`
}

// mergeIndexes combines multiple source indexes into one, tagging each file
// with the root of the index it came from.
func mergeIndexes(indexes []*Index) *Index {
	if len(indexes) == 1 {
		for i := range indexes[0].Files {
			indexes[0].Files[i].SourceIndex = indexes[0].Root
		}
		return indexes[0]
	}
	merged := &Index{Root: "(merged)"}
	for _, idx := range indexes {
		if merged.ScannedAt.IsZero() || idx.ScannedAt.Before(merged.ScannedAt) {
			merged.ScannedAt = idx.ScannedAt
		}
		for _, f := range idx.Files {
			f.SourceIndex = idx.Root
			merged.Files = append(merged.Files, f)
		}
	}
	return merged
}

type CompareResult struct {
	Missing      []FileRecord   `json:"missing"`
	SizeMismatch []MismatchPair `json:"size_mismatch"`
	HashMismatch []MismatchPair `json:"hash_mismatch,omitempty"`
	FuzzyMatch   []FuzzyPair    `json:"fuzzy_match,omitempty"`
}

type MismatchPair struct {
	Source FileRecord `json:"source"`
	Dest   FileRecord `json:"dest"`
}

type FuzzyPair struct {
	Source    FileRecord `json:"source"`
	Dest      FileRecord `json:"dest"`
	SizeMatch bool       `json:"size_match"`
	Reason    string     `json:"reason"` // "substring" or "episode-renumbered"
}

// fmtCount formats an integer with comma thousands separators.
func fmtCount(n int) string {
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// reEpisode matches SxxExx / SxxExxExx patterns (e.g. S01E02, S01E01E02).
var reEpisode = regexp.MustCompile(`(?i)s\d{1,2}e\d{1,2}(?:e\d{1,2})?`)

// stripEpisode removes episode codes and collapses extra whitespace.
func stripEpisode(name string) string {
	s := reEpisode.ReplaceAllString(name, " ")
	return strings.Join(strings.Fields(s), " ")
}

// extractEpisodeKey returns "normalized-show-title::SxxExx" for files that
// contain an episode code, so different encodes of the same episode match.
// Returns "" if no episode code found.
func extractEpisodeKey(name string) string {
	loc := reEpisode.FindStringIndex(name)
	if loc == nil {
		return ""
	}
	code := strings.ToLower(reEpisode.FindString(name))
	// Normalize the first episode code to SxxExx (drop second Exx for multi-ep).
	code = regexp.MustCompile(`(s\d{1,2}e\d{1,2})e\d{1,2}`).ReplaceAllString(code, "$1")
	// Title is everything before the episode code, lowercased and stripped of
	// dots/underscores/brackets so "Mythic.Quest" == "Mythic Quest".
	title := name[:loc[0]]
	title = strings.ToLower(title)
	title = regexp.MustCompile(`[._\[\](){}-]+`).ReplaceAllString(title, " ")
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	return title + "::" + code
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "scan":
		cmdScan(os.Args[2:])
	case "compare":
		cmdCompare(os.Args[2:])
	case "sync-check":
		cmdSyncCheck(os.Args[2:])
	case "version", "--version", "-version":
		fmt.Printf("file-syncer-cmp %s\n", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`file-syncer-cmp - compare media files between two locations

COMMANDS:
  scan         Scan a directory and produce an index file
  compare      Compare index files and report missing/mismatched files
  sync-check   Scan paths directly and compare (both must be accessible)
  version      Print version

EXAMPLES:
  # On transmission server (or mount the disk):
  file-syncer-cmp scan /mnt/disk1 --output disk1.json
  file-syncer-cmp scan /mnt/disk2 --output disk2.json

  # On NAS (or mount the NAS share):
  file-syncer-cmp scan /mnt/nas --output nas.json

  # Compare one source against dest:
  file-syncer-cmp compare disk1.json nas.json

  # Compare multiple sources against one dest:
  file-syncer-cmp compare --dest nas.json disk1.json disk2.json disk3.json

  # Or if both paths are accessible at once:
  file-syncer-cmp sync-check /mnt/disk1 /mnt/nas

  # Multiple source paths:
  file-syncer-cmp sync-check --dest /mnt/nas /mnt/disk1 /mnt/disk2

  # With hash verification (slower but more accurate):
  file-syncer-cmp scan /mnt/disk1 --output disk1.json --hash
  file-syncer-cmp compare disk1.json nas.json --hash

  # Scan multiple NAS paths into one index:
  file-syncer-cmp scan /volume2/data /volume1/home --output nas.json

  # Custom extensions:
  file-syncer-cmp scan /mnt/disk1 --ext mp3,flac,mkv,mp4

  # HTML report:
  file-syncer-cmp compare --dest nas.json disk1.json disk2.json --format html > report.html

  # Fuzzy match (catches renamed/renumbered files on dest):
  file-syncer-cmp compare --dest nas.json disk1.json --fuzzy

  # Filter junk files from missing list:
  file-syncer-cmp compare --dest nas.json disk1.json --ignore 'RARBG*,www.*,*.nfo'

  # Interactive TUI to selectively rsync missing dirs:
  file-syncer-cmp compare --dest nas.json disk1.json --fuzzy --tui
  file-syncer-cmp compare --dest nas.json disk1.json --fuzzy --tui --rsync-script /path/to/rsync.sh
`)
}

// ---------- scan ----------

func cmdScan(args []string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	output := fs.String("output", "", "output JSON file (default: stdout)")
	extList := fs.String("ext", "", "comma-separated extensions (default: media files)")
	doHash := fs.Bool("hash", false, "compute partial file hash (slower)")
	workers := fs.Int("workers", 8, "parallel scan workers")
	incremental := fs.Bool("incremental", false, "reuse unchanged dirs from previous --output index")

	flagArgs, posArgs := splitArgs(args)
	fs.Parse(flagArgs)

	if len(posArgs) < 1 {
		fmt.Fprintln(os.Stderr, "usage: scan <path> [path2 ...] [flags]")
		os.Exit(1)
	}

	exts := parseExts(*extList)

	// Load existing index for incremental mode (only works with --output).
	var baseIndex *Index
	if *incremental && *output != "" {
		if data, err := os.ReadFile(*output); err == nil {
			var old Index
			if json.Unmarshal(data, &old) == nil {
				baseIndex = &old
				fmt.Fprintf(os.Stderr, "\033[1mincremental\033[0m  %s  \033[90m%s files · scanned %s\033[0m\n",
					*output, fmtCount(len(old.Files)), old.ScannedAt.Format("2006-01-02 15:04"))
			}
		}
	}

	t0 := time.Now()
	fmt.Fprintln(os.Stderr)
	var indexes []*Index
	for _, root := range posArgs {
		rt := time.Now()
		fmt.Fprintf(os.Stderr, "  scanning %s ...", root)
		// For multi-root incremental, filter base to this root only.
		var base *Index
		if baseIndex != nil {
			var kept []FileRecord
			for _, f := range baseIndex.Files {
				if f.SourceIndex == root || baseIndex.Root == root {
					kept = append(kept, f)
				}
			}
			base = &Index{Root: root, ScannedAt: baseIndex.ScannedAt, Files: kept}
		}
		idx, err := scanWithBase(root, exts, *doHash, *workers, base)
		if err != nil {
			fatal(err)
		}
		elapsed := time.Since(rt).Round(time.Millisecond)
		fmt.Fprintf(os.Stderr, "\r  \033[36m%-42s\033[0m  \033[1m%s files\033[0m  \033[90m%s\033[0m\n",
			root, fmtCount(len(idx.Files)), elapsed)
		indexes = append(indexes, idx)
	}

	var combined *Index
	if len(indexes) == 1 {
		combined = indexes[0]
	} else {
		combined = mergeIndexes(indexes)
		// For a multi-root scan index, record all roots in Root field.
		roots := make([]string, len(indexes))
		for i, idx := range indexes {
			roots[i] = idx.Root
		}
		combined.Root = strings.Join(roots, ", ")
	}

	data, err := json.MarshalIndent(combined, "", "  ")
	if err != nil {
		fatal(err)
	}

	dest := "(stdout)"
	if *output == "" {
		os.Stdout.Write(data)
		fmt.Println()
	} else {
		if err := os.WriteFile(*output, data, 0644); err != nil {
			fatal(err)
		}
		dest = *output
	}

	total := fmtCount(len(combined.Files))
	elapsed := time.Since(t0).Round(time.Millisecond)
	fmt.Fprintln(os.Stderr)
	if len(indexes) > 1 {
		fmt.Fprintf(os.Stderr, "\033[1;32m✓\033[0m  \033[1m%s files\033[0m across %d roots  \033[90m→ %s\033[0m  \033[1m%s\033[0m\n",
			total, len(indexes), dest, elapsed)
	} else {
		fmt.Fprintf(os.Stderr, "\033[1;32m✓\033[0m  \033[1m%s files\033[0m  \033[90m→ %s\033[0m  \033[1m%s\033[0m\n",
			total, dest, elapsed)
	}
}

// ---------- compare ----------

func cmdCompare(args []string) {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	format := fs.String("format", "text", "output format: text, json, html")
	doHash := fs.Bool("hash", false, "also compare hashes (both indexes must have hashes)")
	doFuzzy := fs.Bool("fuzzy", false, "fuzzy name match: treat dest file as found if src name is substring of dest name")
	doTUI := fs.Bool("tui", false, "interactive TUI to select and run rsync for missing dirs")
	rsyncScript := fs.String("rsync-script", "", "script to run for each selected dir (default: ~/rsync.sh)")
	ignoreFlag := fs.String("ignore", "", "comma-separated filename globs to exclude from missing (e.g. 'RARBG*,www.*.mp4')")
	noSelectFlag := fs.String("no-select", "*radarr*,*sonarr*", "comma-separated dir-name globs shown in TUI but not selectable")
	destFlag := fs.String("dest", "", "destination index (required when passing multiple sources)")

	flagArgs, posArgs := splitArgs(args)
	fs.Parse(flagArgs)

	var srcIndexes []*Index
	var dst *Index

	switch {
	case *destFlag != "":
		if len(posArgs) < 1 {
			fmt.Fprintln(os.Stderr, "usage: compare --dest <dest.json> <src1.json> [src2.json ...]")
			os.Exit(1)
		}
		dst = loadIndex(*destFlag)
		for _, p := range posArgs {
			srcIndexes = append(srcIndexes, loadIndex(p))
		}
	case len(posArgs) == 2:
		srcIndexes = []*Index{loadIndex(posArgs[0])}
		dst = loadIndex(posArgs[1])
	default:
		fmt.Fprintln(os.Stderr, "usage: compare <src.json> <dest.json>")
		fmt.Fprintln(os.Stderr, "       compare --dest <dest.json> <src1.json> [src2.json ...]")
		os.Exit(1)
	}

	src := mergeIndexes(srcIndexes)
	ct := time.Now()
	result := compare(src, dst, *doHash, *doFuzzy)
	filterIgnored(result, *ignoreFlag)
	fmt.Fprintf(os.Stderr, "\033[1;32m✓\033[0m  \033[1m%s\033[0m  \033[31m%s missing\033[0m  \033[33m%s fuzzy\033[0m  \033[90m%s size-mismatch\033[0m\n",
		time.Since(ct).Round(time.Millisecond),
		fmtCount(len(result.Missing)), fmtCount(len(result.FuzzyMatch)), fmtCount(len(result.SizeMismatch)))
	if *doTUI {
		runTUI(result, resolveRsyncScript(*rsyncScript), *noSelectFlag)
	} else {
		printReport(result, src, dst, *format)
	}
}

// ---------- sync-check ----------

func cmdSyncCheck(args []string) {
	fs := flag.NewFlagSet("sync-check", flag.ExitOnError)
	extList := fs.String("ext", "", "comma-separated extensions")
	doHash := fs.Bool("hash", false, "compute and compare hashes")
	doFuzzy := fs.Bool("fuzzy", false, "fuzzy name match: treat dest file as found if src name is substring of dest name")
	doTUI := fs.Bool("tui", false, "interactive TUI to select and run rsync for missing dirs")
	rsyncScript := fs.String("rsync-script", "", "script to run for each selected dir (default: ~/rsync.sh)")
	ignoreFlag := fs.String("ignore", "", "comma-separated filename globs to exclude from missing (e.g. 'RARBG*,www.*.mp4')")
	noSelectFlag := fs.String("no-select", "*radarr*,*sonarr*", "comma-separated dir-name globs shown in TUI but not selectable")
	format := fs.String("format", "text", "output format: text, json, html")
	workers := fs.Int("workers", 8, "parallel scan workers")
	destFlag := fs.String("dest", "", "destination path (required when passing multiple sources)")

	flagArgs, posArgs := splitArgs(args)
	fs.Parse(flagArgs)

	exts := parseExts(*extList)

	var srcPaths []string
	var destPath string

	switch {
	case *destFlag != "":
		if len(posArgs) < 1 {
			fmt.Fprintln(os.Stderr, "usage: sync-check --dest <dest-path> <src1> [src2 ...]")
			os.Exit(1)
		}
		destPath = *destFlag
		srcPaths = posArgs
	case len(posArgs) == 2:
		srcPaths = posArgs[:1]
		destPath = posArgs[1]
	default:
		fmt.Fprintln(os.Stderr, "usage: sync-check <src-path> <dest-path>")
		fmt.Fprintln(os.Stderr, "       sync-check --dest <dest-path> <src1> [src2 ...]")
		os.Exit(1)
	}

	sc0 := time.Now()
	var srcIndexes []*Index
	for _, p := range srcPaths {
		rt := time.Now()
		fmt.Fprintf(os.Stderr, "  scanning %s ...", p)
		idx, err := scan(p, exts, *doHash, *workers)
		if err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stderr, "\r  \033[36m%-42s\033[0m  \033[1m%s files\033[0m  \033[90m%s\033[0m\n",
			p, fmtCount(len(idx.Files)), time.Since(rt).Round(time.Millisecond))
		srcIndexes = append(srcIndexes, idx)
	}

	dt := time.Now()
	fmt.Fprintf(os.Stderr, "  scanning %s ...", destPath)
	dst, err := scan(destPath, exts, *doHash, *workers)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "\r  \033[36m%-42s\033[0m  \033[1m%s files\033[0m  \033[90m%s\033[0m\n",
		destPath, fmtCount(len(dst.Files)), time.Since(dt).Round(time.Millisecond))
	fmt.Fprintf(os.Stderr, "\033[1;32m✓\033[0m  scan done  \033[1m%s\033[0m\n", time.Since(sc0).Round(time.Millisecond))

	src := mergeIndexes(srcIndexes)
	ct := time.Now()
	result := compare(src, dst, *doHash, *doFuzzy)
	filterIgnored(result, *ignoreFlag)
	fmt.Fprintf(os.Stderr, "\033[1;32m✓\033[0m  \033[1m%s\033[0m  \033[31m%s missing\033[0m  \033[33m%s fuzzy\033[0m  \033[90m%s size-mismatch\033[0m\n",
		time.Since(ct).Round(time.Millisecond),
		fmtCount(len(result.Missing)), fmtCount(len(result.FuzzyMatch)), fmtCount(len(result.SizeMismatch)))
	if *doTUI {
		runTUI(result, resolveRsyncScript(*rsyncScript), *noSelectFlag)
	} else {
		printReport(result, src, dst, *format)
	}
}

// ---------- scan implementation ----------

func scan(root string, exts map[string]bool, doHash bool, workers int) (*Index, error) {
	return scanWithBase(root, exts, doHash, workers, nil)
}

func scanWithBase(root string, exts map[string]bool, doHash bool, workers int, base *Index) (*Index, error) {
	// Build per-dir lookup from old index for incremental mode.
	type job struct {
		path string
		info os.FileInfo
	}

	var lastScan time.Time
	oldByDir := map[string][]FileRecord{} // rel dir → records
	if base != nil {
		lastScan = base.ScannedAt
		for _, f := range base.Files {
			d := filepath.Dir(f.Path)
			oldByDir[d] = append(oldByDir[d], f)
		}
	}

	jobs := make(chan job, 256)
	results := make(chan FileRecord, 256)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				rec := FileRecord{
					Path:     j.path,
					Name:     j.info.Name(),
					Size:     j.info.Size(),
					Ext:      strings.ToLower(strings.TrimPrefix(filepath.Ext(j.info.Name()), ".")),
					Modified: j.info.ModTime().Unix(),
				}
				if doHash {
					rec.Hash = partialHash(j.path)
				}
				results <- rec
			}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	// reused holds records copied from the old index without rescanning.
	var reusedMu sync.Mutex
	var reused []FileRecord
	skippedDirs := 0

	go func() {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				if base == nil {
					return nil
				}
				// Skip rescanning this dir if it hasn't changed since last scan.
				if info.ModTime().Before(lastScan) {
					rel, _ := filepath.Rel(root, path)
					if old, ok := oldByDir[rel]; ok {
						reusedMu.Lock()
						reused = append(reused, old...)
						skippedDirs++
						reusedMu.Unlock()
						return filepath.SkipDir
					}
				}
				return nil
			}
			ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(info.Name()), "."))
			if len(exts) == 0 || exts[ext] {
				rel, _ := filepath.Rel(root, path)
				jobs <- job{rel, info}
			}
			return nil
		})
		close(jobs)
	}()

	var files []FileRecord
	for r := range results {
		files = append(files, r)
	}
	files = append(files, reused...)

	if base != nil && skippedDirs > 0 {
		fmt.Fprintf(os.Stderr, "incremental: skipped %d unchanged dirs, rescanned %d files\n",
			skippedDirs, len(files)-len(reused))
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})

	return &Index{
		Root:      root,
		ScannedAt: time.Now(),
		Files:     files,
	}, nil
}

// partialHash hashes first 512KB + last 512KB + size for speed on large files.
func partialHash(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0
	}

	h := fnv.New64a()
	const chunk = 512 * 1024
	buf := make([]byte, chunk)

	n, _ := io.ReadFull(f, buf)
	h.Write(buf[:n])

	if info.Size() > chunk*2 {
		f.Seek(-chunk, io.SeekEnd)
		n, _ = io.ReadFull(f, buf)
		h.Write(buf[:n])
	}

	var sizeBuf [8]byte
	for i := 0; i < 8; i++ {
		sizeBuf[i] = byte(info.Size() >> (i * 8))
	}
	h.Write(sizeBuf[:])

	return h.Sum64()
}

// ---------- compare implementation ----------

func compare(src, dst *Index, useHash bool, fuzzy bool) *CompareResult {
	byName := make(map[string][]FileRecord, len(dst.Files))
	for _, f := range dst.Files {
		key := strings.ToLower(f.Name)
		byName[key] = append(byName[key], f)
	}

	// Pre-compute fuzzy indexes once — O(m) setup, O(1) or O(k) lookup per miss.
	var (
		allDest      []FileRecord
		destLow      []string         // lowercased name, parallel to allDest
		byStripped   map[string]int   // stripped name → first allDest index (O(1) tier-2)
		byEpisodeKey map[string]int   // "title::SxxExx" → first allDest index (O(1) tier-3)
		destByExt    map[string][]int // ext → allDest indices (narrows tier-1 scan)
	)
	if fuzzy {
		allDest = dst.Files
		destLow = make([]string, len(allDest))
		byStripped = make(map[string]int, len(allDest))
		byEpisodeKey = make(map[string]int, len(allDest))
		destByExt = make(map[string][]int, 32)
		for i, f := range allDest {
			destLow[i] = strings.ToLower(f.Name)
			s := strings.ToLower(stripEpisode(f.Name))
			if _, exists := byStripped[s]; !exists {
				byStripped[s] = i
			}
			ext := strings.ToLower(f.Ext)
			destByExt[ext] = append(destByExt[ext], i)
			if ek := extractEpisodeKey(f.Name); ek != "" {
				if _, exists := byEpisodeKey[ek]; !exists {
					byEpisodeKey[ek] = i
				}
			}
		}
	}

	result := &CompareResult{}

	for _, sf := range src.Files {
		key := strings.ToLower(sf.Name)
		candidates, found := byName[key]
		if !found {
			// Try fuzzy: src name is a substring of some dest name (or vice versa).
			if fuzzy {
				srcLow := strings.ToLower(sf.Name)
				srcExt := strings.ToLower(sf.Ext)

				// Tier 1: substring match — only scan dest files with same extension.
				var best *FileRecord
				var reason string
				candidates1 := destByExt[srcExt] // pre-filtered by ext
				if len(candidates1) == 0 {
					candidates1 = make([]int, len(allDest)) // fallback: all
					for i := range allDest {
						candidates1[i] = i
					}
				}
				for _, i := range candidates1 {
					dLow := destLow[i]
					if strings.Contains(dLow, srcLow) || strings.Contains(srcLow, dLow) {
						f := allDest[i]
						best = &f
						reason = "substring"
						break
					}
				}

				// Tier 2: O(1) map lookup on stripped name (handles episode renumbering).
				if best == nil {
					srcStripped := strings.ToLower(stripEpisode(sf.Name))
					if srcStripped != "" {
						if idx, ok := byStripped[srcStripped]; ok {
							f := allDest[idx]
							best = &f
							reason = "episode-renumbered"
						}
					}
				}

				// Tier 3: match on show-title + episode code only, ignoring encode/group.
				// Catches same episode available in a different encode on dest.
				if best == nil {
					if ek := extractEpisodeKey(sf.Name); ek != "" {
						if idx, ok := byEpisodeKey[ek]; ok {
							f := allDest[idx]
							best = &f
							reason = "episode-reencoded"
						}
					}
				}

				if best != nil {
					result.FuzzyMatch = append(result.FuzzyMatch, FuzzyPair{
						Source:    sf,
						Dest:      *best,
						SizeMatch: sf.Size == best.Size,
						Reason:    reason,
					})
					continue
				}
			}
			result.Missing = append(result.Missing, sf)
			continue
		}

		var sizeMatch *FileRecord
		for i := range candidates {
			if candidates[i].Size == sf.Size {
				sizeMatch = &candidates[i]
				break
			}
		}

		if sizeMatch == nil {
			result.SizeMismatch = append(result.SizeMismatch, MismatchPair{
				Source: sf,
				Dest:   candidates[0],
			})
			continue
		}

		if useHash && sf.Hash != 0 && sizeMatch.Hash != 0 && sf.Hash != sizeMatch.Hash {
			result.HashMismatch = append(result.HashMismatch, MismatchPair{
				Source: sf,
				Dest:   *sizeMatch,
			})
		}
	}

	return result
}

// resolveRsyncScript returns the script path to use, expanding ~ if present.
// If empty, defaults to ~/rsync.sh.
func resolveRsyncScript(s string) string {
	if s == "" {
		s = "~/rsync.sh"
	}
	if strings.HasPrefix(s, "~/") {
		s = filepath.Join(os.Getenv("HOME"), s[2:])
	}
	return s
}

// ---------- TUI ----------

type tuiDir struct {
	disk     string
	dir      string
	count    int
	disabled bool // visible but not selectable (e.g. auto-managed dirs)
}

func buildTUIDirs(missing []FileRecord) []tuiDir {
	type key struct{ disk, dir string }
	var order []key
	seen := map[key]bool{}
	counts := map[key]int{}
	for _, f := range missing {
		root := f.SourceIndex
		if root == "" {
			root = "."
		}
		dir := filepath.Dir(filepath.Join(root, f.Path))
		k := key{root, dir}
		if !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
		counts[k]++
	}
	raw := make([]tuiDir, len(order))
	for i, k := range order {
		raw[i] = tuiDir{disk: k.disk, dir: k.dir, count: counts[k]}
	}

	// Drop any entry whose path is a strict parent of another entry.
	// These are scan roots with loose files directly inside — not useful
	// as rsync targets when subdirectories are also listed.
	dirSet := make(map[string]bool, len(raw))
	for _, d := range raw {
		dirSet[d.dir] = true
	}
	var dirs []tuiDir
	for _, d := range raw {
		isParent := false
		for other := range dirSet {
			if other != d.dir && strings.HasPrefix(other, d.dir+string(filepath.Separator)) {
				isParent = true
				break
			}
		}
		if !isParent {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func runTUI(result *CompareResult, rsyncScript string, noSelect string) {
	if len(result.Missing) == 0 {
		fmt.Fprintln(os.Stderr, "No missing files.")
		return
	}

	dirs := buildTUIDirs(result.Missing)

	// Mark dirs whose base name matches any --no-select glob as disabled.
	if noSelect != "" {
		patterns := strings.Split(noSelect, ",")
		for i, d := range dirs {
			base := filepath.Base(d.dir)
			for _, pat := range patterns {
				pat = strings.TrimSpace(pat)
				if matched, _ := filepath.Match(strings.ToLower(pat), strings.ToLower(base)); matched {
					dirs[i].disabled = true
					break
				}
			}
		}
	}
	selected := make([]bool, len(dirs))
	cursor := 0
	viewTop := 0

	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		fatal(fmt.Errorf("cannot enter raw mode: %w", err))
	}
	restore := func() { term.Restore(fd, oldState) }
	defer restore()

	out := os.Stdout
	buf := make([]byte, 4)

	draw := func() {
		termW, termH, _ := term.GetSize(int(out.Fd()))
		if termW < 40 {
			termW = 80
		}
		if termH < 8 {
			termH = 24
		}
		headerLines := 4
		visible := termH - headerLines - 2
		if visible < 1 {
			visible = 1
		}

		// Scroll viewport to keep cursor visible.
		if cursor < viewTop {
			viewTop = cursor
		}
		if cursor >= viewTop+visible {
			viewTop = cursor - visible + 1
		}

		fmt.Fprint(out, "\033[H\033[2J") // clear

		nSel := 0
		for _, s := range selected {
			if s {
				nSel++
			}
		}

		fmt.Fprintf(out, "\033[1;36m File Sync TUI\033[0m  %d missing files · %d dirs · \033[33m%d selected\033[0m\r\n", len(result.Missing), len(dirs), nSel)
		fmt.Fprintf(out, "\033[90m ↑↓ move · SPACE toggle · a=all  n=none · ENTER run · q quit\033[0m\r\n")
		fmt.Fprintf(out, "\033[90m%s\033[0m\r\n", strings.Repeat("─", termW-1))

		end := viewTop + visible
		if end > len(dirs) {
			end = len(dirs)
		}
		for i := viewTop; i < end; i++ {
			d := dirs[i]
			var check, cc string
			if d.disabled {
				check = "[-]"
				cc = "\033[90m" // dim — not selectable
			} else if selected[i] {
				check = "[✓]"
				cc = "\033[32m"
			} else {
				check = "[ ]"
				cc = "\033[90m"
			}
			label := d.dir
			maxLabel := termW - 14
			if len(label) > maxLabel {
				label = "…" + label[len(label)-maxLabel+1:]
			}
			if i == cursor {
				// Subtle blue-gray bg, bold label, cyan arrow prefix.
				fmt.Fprintf(out, "\033[48;5;237m\033[1m\033[36m▶ \033[0m\033[48;5;237m%s%s\033[0m\033[48;5;237m %s  \033[90m(%d)\033[0m\r\n",
					cc, check, label, d.count)
			} else {
				fmt.Fprintf(out, "  %s%s\033[0m %s  \033[90m(%d)\033[0m\r\n", cc, check, label, d.count)
			}
		}

		if len(dirs) > visible {
			fmt.Fprintf(out, "\033[90m  [%d–%d of %d]\033[0m\r\n", viewTop+1, end, len(dirs))
		}
	}

	for {
		draw()
		n, _ := os.Stdin.Read(buf)
		if n == 0 {
			continue
		}
		b := buf[0]
		switch {
		case b == 'q' || b == 'Q' || b == 3: // q / Ctrl-C
			restore()
			fmt.Print("\033[H\033[2J")
			return
		case b == ' ':
			if !dirs[cursor].disabled {
				selected[cursor] = !selected[cursor]
			}
		case b == 'a' || b == 'A':
			for i := range selected {
				if !dirs[i].disabled {
					selected[i] = true
				}
			}
		case b == 'n' || b == 'N':
			for i := range selected {
				selected[i] = false
			}
		case b == 0x1b && n >= 3 && buf[1] == '[':
			switch buf[2] {
			case 'A': // up
				if cursor > 0 {
					cursor--
				}
			case 'B': // down
				if cursor < len(dirs)-1 {
					cursor++
				}
			}
		case b == '\r' || b == '\n':
			restore()
			fmt.Print("\033[H\033[2J")
			ran := tuiRunSelected(dirs, selected, rsyncScript)
			// Remove synced dirs and re-enter TUI with remainder.
			if len(ran) > 0 {
				var remaining []tuiDir
				for i, d := range dirs {
					if !ran[i] {
						remaining = append(remaining, d)
					}
				}
				dirs = remaining
				selected = make([]bool, len(dirs))
				if cursor >= len(dirs) {
					cursor = len(dirs) - 1
				}
				if cursor < 0 {
					cursor = 0
				}
				viewTop = 0
				if len(dirs) == 0 {
					fmt.Println("All selected dirs synced.")
					return
				}
				oldState2, err2 := term.MakeRaw(fd)
				if err2 != nil {
					return
				}
				oldState = oldState2
				restore = func() { term.Restore(fd, oldState) }
				continue
			}
			return
		}
	}
}

// tuiRunSelected runs rsync for each selected dir, prompting per-dir unless
// "always" mode is active. Returns a map[original index]bool of dirs that were
// actually run (so the caller can remove them from the list).
func tuiRunSelected(dirs []tuiDir, selected []bool, rsyncScript string) map[int]bool {
	any := false
	for _, s := range selected {
		if s {
			any = true
			break
		}
	}
	if !any {
		fmt.Println("Nothing selected.")
		return nil
	}
	sep := strings.Repeat("─", 60)
	skipped := 0
	always := false
	ran := map[int]bool{}

	// Coalesce siblings: if >1 selected dirs share the same parent, rsync the
	// parent once instead of each child separately. Repeat until stable so
	// deeply nested groups (e.g. artist/album/*) collapse all the way up.
	type runItem struct {
		path    string // path passed to rsync script
		indices []int  // original dir indices this item covers
	}

	// Seed with one item per selected dir.
	var items []runItem
	for i, d := range dirs {
		if selected[i] {
			items = append(items, runItem{path: d.dir, indices: []int{i}})
		}
	}

	// Iteratively collapse siblings to their parent (max 8 passes).
	// Stop collapsing when the parent has depth ≤ 2 (e.g. /mnt or /mnt/disk)
	// to avoid coalescing into a filesystem root.
	isTooShallow := func(p string) bool {
		clean := filepath.Clean(p)
		return strings.Count(clean, string(filepath.Separator)) <= 2
	}
	for pass := 0; pass < 8; pass++ {
		parentCount := map[string]int{}
		for _, it := range items {
			parent := filepath.Dir(it.path)
			if !isTooShallow(parent) {
				parentCount[parent]++
			}
		}
		merged := map[string]*runItem{}
		var next []runItem
		changed := false
		for _, it := range items {
			parent := filepath.Dir(it.path)
			if parentCount[parent] > 1 {
				changed = true
				if m, ok := merged[parent]; ok {
					m.indices = append(m.indices, it.indices...)
				} else {
					cp := runItem{path: parent, indices: append([]int(nil), it.indices...)}
					merged[parent] = &cp
					next = append(next, cp)
				}
			} else {
				next = append(next, it)
			}
		}
		// Rebuild with merged pointers reflected.
		items = next[:0]
		seen := map[string]bool{}
		for _, it := range next {
			if merged[it.path] != nil {
				if !seen[it.path] {
					seen[it.path] = true
					items = append(items, *merged[it.path])
				}
			} else {
				items = append(items, it)
			}
		}
		if !changed {
			break
		}
	}

	for itemIdx, item := range items {
		fmt.Printf("\n%s\n\033[1;33m%s\033[0m\n", sep, item.path)
		if len(item.indices) > 1 {
			fmt.Printf("\033[90m(%d dirs coalesced to parent)\033[0m\n", len(item.indices))
		}
		fmt.Printf("\033[90m%s %q\033[0m\n", rsyncScript, item.path)

		run := always
		if !always {
			fmt.Printf("Run? [y/a/N/q] ")
			var resp [1]byte
			os.Stdin.Read(resp[:])
			// Drain the rest of the line (\n left after single-key read).
			var drain [256]byte
			for {
				n, _ := os.Stdin.Read(drain[:])
				done := false
				for _, b := range drain[:n] {
					if b == '\n' || b == '\r' {
						done = true
					}
				}
				if done || n == 0 {
					break
				}
			}
			fmt.Println()
			switch resp[0] {
			case 'y', 'Y':
				run = true
			case 'a', 'A':
				run = true
				always = true
				fmt.Println("\033[90mRunning all remaining...\033[0m")
			case 'q', 'Q', 3:
				fmt.Printf("Quit. (%d remaining skipped)\n", len(items)-itemIdx)
				fmt.Printf("\n%s\nDone. %d skipped.\nPress Enter to return to TUI...", sep, skipped)
				var b [256]byte
				os.Stdin.Read(b[:])
				return ran
			default:
				skipped++
				fmt.Println("\033[90mSkipped.\033[0m")
			}
		}

		if run {
			for _, idx := range item.indices {
				ran[idx] = true
			}
			cmd := exec.Command(rsyncScript, item.path)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "\033[31mrsync failed:\033[0m %v\n", err)
			}
		}
	}
	fmt.Printf("\n%s\nDone. %d skipped.\nPress Enter to return to TUI...", sep, skipped)
	var b [256]byte
	os.Stdin.Read(b[:])
	return ran
}

// filterIgnored removes entries from result.Missing whose filename matches any
// of the comma-separated glob patterns in the ignore string. Non-glob patterns
// are treated as exact filename matches. Case-insensitive.
func filterIgnored(result *CompareResult, ignore string) {
	if ignore == "" {
		return
	}
	patterns := strings.Split(ignore, ",")
	for i, p := range patterns {
		patterns[i] = strings.ToLower(strings.TrimSpace(p))
	}

	keep := result.Missing[:0]
	for _, f := range result.Missing {
		nameLow := strings.ToLower(f.Name)
		matched := false
		for _, p := range patterns {
			if ok, _ := filepath.Match(p, nameLow); ok {
				matched = true
				break
			}
		}
		if !matched {
			keep = append(keep, f)
		}
	}
	result.Missing = keep
}

// rsyncCommands builds deduplicated ~/rsync.sh commands for missing files,
// grouped by source disk. Each command syncs the parent directory of the file.
func rsyncCommands(missing []FileRecord) string {
	// disk → ordered unique dirs
	type entry struct{ disk, dir string }
	seen := map[entry]bool{}
	var order []entry

	for _, f := range missing {
		root := f.SourceIndex
		if root == "" {
			root = "."
		}
		dir := filepath.Dir(filepath.Join(root, f.Path))
		e := entry{root, dir}
		if !seen[e] {
			seen[e] = true
			order = append(order, e)
		}
	}

	// Group by disk for readability
	byDisk := map[string][]string{}
	var diskOrder []string
	seenDisk := map[string]bool{}
	for _, e := range order {
		if !seenDisk[e.disk] {
			seenDisk[e.disk] = true
			diskOrder = append(diskOrder, e.disk)
		}
		byDisk[e.disk] = append(byDisk[e.disk], e.dir)
	}

	var b strings.Builder
	for _, disk := range diskOrder {
		fmt.Fprintf(&b, "# %s\n", disk)
		for _, dir := range byDisk[disk] {
			fmt.Fprintf(&b, "~/rsync.sh %q\n", dir)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---------- reporting ----------

func printReport(result *CompareResult, src, dst *Index, format string) {
	switch format {
	case "json":
		data, _ := json.MarshalIndent(result, "", "  ")
		os.Stdout.Write(data)
		fmt.Println()
	case "html":
		printHTML(result, src, dst)
	default:
		printText(result, src, dst)
	}
}

func printText(result *CompareResult, src, dst *Index) {
	w := os.Stdout
	sep := strings.Repeat("-", 80)
	multiSrc := src.Root == "(merged)"

	fmt.Fprintf(w, "\nFILE SYNC REPORT\n%s\n", sep)
	if multiSrc {
		seen := map[string]bool{}
		for _, f := range src.Files {
			seen[f.SourceIndex] = true
		}
		roots := make([]string, 0, len(seen))
		for r := range seen {
			roots = append(roots, r)
		}
		sort.Strings(roots)
		fmt.Fprintf(w, "Sources: %s\n", strings.Join(roots, ", "))
		fmt.Fprintf(w, "         (%d files total)\n", len(src.Files))
	} else {
		fmt.Fprintf(w, "Source:  %s (scanned %s, %d files)\n", src.Root, src.ScannedAt.Format("2006-01-02 15:04"), len(src.Files))
	}
	fmt.Fprintf(w, "Dest:    %s (scanned %s, %d files)\n", dst.Root, dst.ScannedAt.Format("2006-01-02 15:04"), len(dst.Files))
	fmt.Fprintf(w, "%s\n\n", sep)

	if len(result.Missing) == 0 && len(result.SizeMismatch) == 0 && len(result.HashMismatch) == 0 {
		fmt.Fprintln(w, "ALL FILES PRESENT - no issues found")
		return
	}

	if len(result.Missing) > 0 {
		fmt.Fprintf(w, "MISSING FILES (%d) - present in source, not found in dest:\n%s\n", len(result.Missing), sep)
		for _, f := range result.Missing {
			if multiSrc {
				fmt.Fprintf(w, "  [MISSING] %s  (%s)  [from: %s]\n", f.Path, humanSize(f.Size), f.SourceIndex)
			} else {
				fmt.Fprintf(w, "  [MISSING] %s  (%s)\n", f.Path, humanSize(f.Size))
			}
		}
		fmt.Fprintln(w)
	}

	if len(result.SizeMismatch) > 0 {
		fmt.Fprintf(w, "SIZE MISMATCH (%d) - name found in dest but different size:\n%s\n", len(result.SizeMismatch), sep)
		for _, p := range result.SizeMismatch {
			fmt.Fprintf(w, "  [MISMATCH] %s\n", p.Source.Name)
			fmt.Fprintf(w, "    src: %s  (%s)\n", p.Source.Path, humanSize(p.Source.Size))
			fmt.Fprintf(w, "    dst: %s  (%s)\n", p.Dest.Path, humanSize(p.Dest.Size))
		}
		fmt.Fprintln(w)
	}

	if len(result.HashMismatch) > 0 {
		fmt.Fprintf(w, "HASH MISMATCH (%d) - same name+size but different content:\n%s\n", len(result.HashMismatch), sep)
		for _, p := range result.HashMismatch {
			fmt.Fprintf(w, "  [CORRUPT?] %s\n", p.Source.Name)
			fmt.Fprintf(w, "    src: %s\n", p.Source.Path)
			fmt.Fprintf(w, "    dst: %s\n", p.Dest.Path)
		}
		fmt.Fprintln(w)
	}

	if len(result.FuzzyMatch) > 0 {
		fmt.Fprintf(w, "FUZZY MATCH (%d) - likely present but renamed/renumbered:\n%s\n", len(result.FuzzyMatch), sep)
		for _, p := range result.FuzzyMatch {
			sizeTag := "size ok"
			if !p.SizeMatch {
				sizeTag = "SIZE DIFFERS"
			}
			fmt.Fprintf(w, "  [FUZZY/%s/%s] %s\n", p.Reason, sizeTag, p.Source.Name)
			fmt.Fprintf(w, "    src: %s  (%s)\n", p.Source.Path, humanSize(p.Source.Size))
			fmt.Fprintf(w, "    dst: %s  (%s)\n", p.Dest.Path, humanSize(p.Dest.Size))
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "%s\nSUMMARY: %d missing, %d size-mismatch, %d hash-mismatch, %d fuzzy-match\n",
		sep, len(result.Missing), len(result.SizeMismatch), len(result.HashMismatch), len(result.FuzzyMatch))
}

func printHTML(result *CompareResult, src, dst *Index) {
	missing := result.Missing
	sizeMismatch := result.SizeMismatch
	hashMismatch := result.HashMismatch
	multiSrc := src.Root == "(merged)"

	var missingBuf, sizeBuf, hashBuf, fuzzyBuf strings.Builder

	// Group missing files by parent directory for collapsible display.
	if len(missing) > 0 {
		type dirGroup struct {
			dir   string
			files []FileRecord
		}
		dirOrder := []string{}
		dirMap := map[string]*dirGroup{}
		for _, f := range missing {
			d := filepath.Dir(f.Path)
			if d == "." {
				d = "(root)"
			}
			if _, ok := dirMap[d]; !ok {
				dirOrder = append(dirOrder, d)
				dirMap[d] = &dirGroup{dir: d}
			}
			dirMap[d].files = append(dirMap[d].files, f)
		}
		for _, d := range dirOrder {
			g := dirMap[d]
			fmt.Fprintf(&missingBuf, "<details><summary class=\"dir-summary\"><span class=\"dir-name\">%s/</span> <span class=\"dir-count\">%d file(s)</span></summary><table class=\"dir-table\">",
				htmlEsc(g.dir), len(g.files))
			if multiSrc {
				fmt.Fprint(&missingBuf, "<tr><th>File</th><th>Source Disk</th><th>Ext</th><th>Size</th></tr>")
				for _, f := range g.files {
					fmt.Fprintf(&missingBuf, "<tr><td class=\"path\">%s</td><td class=\"dst-path\">%s</td><td class=\"ext\">%s</td><td class=\"size\">%s</td></tr>",
						htmlEsc(f.Name), htmlEsc(f.SourceIndex), htmlEsc(f.Ext), humanSize(f.Size))
				}
			} else {
				fmt.Fprint(&missingBuf, "<tr><th>File</th><th>Ext</th><th>Size</th></tr>")
				for _, f := range g.files {
					fmt.Fprintf(&missingBuf, "<tr><td class=\"path\">%s</td><td class=\"ext\">%s</td><td class=\"size\">%s</td></tr>",
						htmlEsc(f.Name), htmlEsc(f.Ext), humanSize(f.Size))
				}
			}
			fmt.Fprint(&missingBuf, "</table></details>")
		}
	}

	for _, p := range sizeMismatch {
		fmt.Fprintf(&sizeBuf, "<tr><td class=\"name\">%s</td><td class=\"path\">%s<br><span class=\"dst-path\">%s</span></td><td class=\"size\">%s &rarr; %s</td></tr>",
			htmlEsc(p.Source.Name), htmlEsc(p.Source.Path), htmlEsc(p.Dest.Path),
			humanSize(p.Source.Size), humanSize(p.Dest.Size))
	}
	for _, p := range hashMismatch {
		fmt.Fprintf(&hashBuf, "<tr><td class=\"name\">%s</td><td class=\"path\">%s<br><span class=\"dst-path\">%s</span></td></tr>",
			htmlEsc(p.Source.Name), htmlEsc(p.Source.Path), htmlEsc(p.Dest.Path))
	}
	for _, p := range result.FuzzyMatch {
		sizeTag := "<span style=\"color:var(--green)\">size ok</span>"
		if !p.SizeMatch {
			sizeTag = "<span style=\"color:var(--red)\">size differs</span>"
		}
		reasonTag := p.Reason
		fmt.Fprintf(&fuzzyBuf, "<tr><td class=\"name\">%s</td><td class=\"path\">%s<br><span class=\"dst-path\">%s</span></td><td class=\"size\">%s &rarr; %s</td><td>%s</td><td class=\"ext\">%s</td></tr>",
			htmlEsc(p.Source.Name), htmlEsc(p.Source.Path), htmlEsc(p.Dest.Path),
			humanSize(p.Source.Size), humanSize(p.Dest.Size), sizeTag, htmlEsc(reasonTag))
	}
	missingRows := missingBuf.String()
	sizeRows := sizeBuf.String()
	hashRows := hashBuf.String()
	fuzzyRows := fuzzyBuf.String()

	var srcMetaBuf strings.Builder
	if multiSrc {
		seen := map[string]bool{}
		for _, f := range src.Files {
			seen[f.SourceIndex] = true
		}
		roots := make([]string, 0, len(seen))
		for r := range seen {
			roots = append(roots, r)
		}
		sort.Strings(roots)
		for _, r := range roots {
			fmt.Fprintf(&srcMetaBuf, "<b>%s</b><br>", htmlEsc(r))
		}
		fmt.Fprintf(&srcMetaBuf, "(%d files total)", len(src.Files))
	} else {
		fmt.Fprintf(&srcMetaBuf, "<b>%s</b> &middot; %d files &middot; scanned %s",
			htmlEsc(src.Root), len(src.Files), src.ScannedAt.Format("2006-01-02 15:04"))
	}
	srcMeta := srcMetaBuf.String()

	status := "ok"
	statusText := "All files present"
	if len(missing)+len(sizeMismatch)+len(hashMismatch) > 0 {
		status = "warn"
		statusText = fmt.Sprintf("%d missing &middot; %d size-mismatch &middot; %d hash-mismatch",
			len(missing), len(sizeMismatch), len(hashMismatch))
	}

	missingHeader := "Path"
	if multiSrc {
		missingHeader = "Path / Source Disk"
	}

	w := os.Stdout
	fmt.Fprint(w, "<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\"><title>File Sync Report</title><style>\n")
	fmt.Fprint(w, "  :root{--bg:#0f1117;--surface:#1a1d27;--border:#2d3148;--text:#e2e8f0;--muted:#64748b;--red:#f87171;--yellow:#fbbf24;--green:#4ade80;--blue:#60a5fa;}\n")
	fmt.Fprint(w, "  *{box-sizing:border-box;margin:0;padding:0}\n")
	fmt.Fprint(w, "  body{background:var(--bg);color:var(--text);font:14px/1.5 ui-monospace,monospace;padding:24px}\n")
	fmt.Fprint(w, "  h1{font-size:18px;font-weight:600;margin-bottom:4px}\n")
	fmt.Fprint(w, "  .meta{color:var(--muted);font-size:12px;margin-bottom:24px}\n")
	fmt.Fprint(w, "  .status-ok{color:var(--green)}.status-warn{color:var(--red)}\n")
	fmt.Fprint(w, "  .section{margin-bottom:32px}\n")
	fmt.Fprint(w, "  .section h2{font-size:13px;font-weight:600;text-transform:uppercase;letter-spacing:.08em;color:var(--muted);margin-bottom:8px;padding-bottom:6px;border-bottom:1px solid var(--border)}\n")
	fmt.Fprint(w, "  .section h2 .count{color:var(--red);margin-left:8px}\n")
	fmt.Fprint(w, "  table{width:100%;border-collapse:collapse}\n")
	fmt.Fprint(w, "  th{text-align:left;font-size:11px;text-transform:uppercase;letter-spacing:.06em;color:var(--muted);padding:6px 10px;border-bottom:1px solid var(--border)}\n")
	fmt.Fprint(w, "  td{padding:7px 10px;border-bottom:1px solid var(--border);word-break:break-all}\n")
	fmt.Fprint(w, "  tr:hover td{background:var(--surface)}\n")
	fmt.Fprint(w, "  .path{color:var(--blue)}.dst-path{color:var(--muted);font-size:12px}.ext{color:var(--yellow);width:60px}.size{color:var(--muted);white-space:nowrap;width:100px}.name{color:var(--text);width:280px}\n")
	fmt.Fprint(w, "  .empty{color:var(--green);padding:10px}\n")
	fmt.Fprint(w, "  .summary{display:flex;gap:24px;margin-top:24px;padding-top:16px;border-top:1px solid var(--border)}\n")
	fmt.Fprint(w, "  .stat{text-align:center}.stat .n{font-size:28px;font-weight:700}.stat .l{font-size:11px;color:var(--muted);text-transform:uppercase}\n")
	fmt.Fprint(w, "  .n-red{color:var(--red)}.n-yellow{color:var(--yellow)}.n-green{color:var(--green)}\n")
	fmt.Fprint(w, "  details{margin-bottom:4px}\n")
	fmt.Fprint(w, "  .dir-summary{cursor:pointer;list-style:none;padding:7px 10px;border-radius:4px;background:var(--surface);border:1px solid var(--border);display:flex;align-items:center;gap:10px;user-select:none}\n")
	fmt.Fprint(w, "  .dir-summary::-webkit-details-marker{display:none}\n")
	fmt.Fprint(w, "  .dir-summary::before{content:'▶';font-size:10px;color:var(--muted);transition:transform .15s}\n")
	fmt.Fprint(w, "  details[open]>.dir-summary::before{transform:rotate(90deg)}\n")
	fmt.Fprint(w, "  .dir-summary:hover{border-color:var(--blue)}\n")
	fmt.Fprint(w, "  .dir-name{color:var(--blue);font-weight:600}\n")
	fmt.Fprint(w, "  .dir-count{color:var(--muted);font-size:11px}\n")
	fmt.Fprint(w, "  .dir-table{width:100%;border-collapse:collapse;margin:0 0 4px 0;border:1px solid var(--border);border-top:none;border-radius:0 0 4px 4px}\n")
	fmt.Fprint(w, "  .dir-table td,.dir-table th{padding:5px 12px 5px 28px}\n")
	fmt.Fprint(w, "  .rsync-wrap{position:relative}\n")
	fmt.Fprint(w, "  .rsync-pre{background:var(--surface);border:1px solid var(--border);border-radius:6px;padding:14px 16px;overflow-x:auto;font-size:12px;line-height:1.6;color:var(--green);white-space:pre}\n")
	fmt.Fprint(w, "  .copy-btn{position:absolute;top:8px;right:8px;background:var(--border);color:var(--text);border:none;border-radius:4px;padding:4px 10px;font:11px ui-monospace,monospace;cursor:pointer}\n")
	fmt.Fprint(w, "  .copy-btn:hover{background:var(--blue);color:#000}\n")
	fmt.Fprint(w, "</style><script>function copyRsync(id,btn){navigator.clipboard.writeText(document.getElementById(id).textContent).then(function(){btn.textContent='Copied!';setTimeout(function(){btn.textContent='Copy'},2000)}).catch(function(){var el=document.getElementById(id);var r=document.createRange();r.selectNode(el);window.getSelection().removeAllRanges();window.getSelection().addRange(r)})}</script></head><body>\n")
	fmt.Fprint(w, "<h1>File Sync Report</h1>\n")
	fmt.Fprintf(w, "<div class=\"meta\">\n  Source: %s<br>\n  Dest: <b>%s</b> &middot; %d files &middot; scanned %s<br>\n  Status: <span class=\"status-%s\">%s</span>\n</div>\n",
		srcMeta, htmlEsc(dst.Root), len(dst.Files), dst.ScannedAt.Format("2006-01-02 15:04"), status, statusText)
	fmt.Fprintf(w, "<div class=\"section\"><h2>Missing Files<span class=\"count\">%d</span></h2>%s</div>\n",
		len(missing), missingTableOrEmpty(missingRows, missingHeader, multiSrc))
	fmt.Fprintf(w, "<div class=\"section\"><h2>Size Mismatch<span class=\"count\">%d</span></h2>%s</div>\n",
		len(sizeMismatch), sizeTableOrEmpty(sizeRows))
	fmt.Fprint(w, hashSection(hashRows, len(hashMismatch)))
	if fuzzyRows != "" {
		fmt.Fprintf(w, "<div class=\"section\"><h2>Fuzzy Match — likely renamed/renumbered<span class=\"count\" style=\"color:var(--blue)\">%d</span></h2><p style=\"color:var(--muted);font-size:12px;margin-bottom:8px\">name or title matches dest file — likely copied but renamed or episode renumbered</p><table><tr><th>Name</th><th>Paths (src / dest)</th><th>Size</th><th>Size Check</th><th>Reason</th></tr>%s</table></div>\n",
			len(result.FuzzyMatch), fuzzyRows)
	}
	if len(missing) > 0 {
		cmds := htmlEsc(rsyncCommands(missing))
		fmt.Fprintf(w, "<div class=\"section\"><h2>Rsync Commands<span class=\"count\" style=\"color:var(--green)\">%d dirs</span></h2><p style=\"color:var(--muted);font-size:12px;margin-bottom:8px\">Run ~/rsync.sh for each missing-file directory:</p><div class=\"rsync-wrap\"><pre class=\"rsync-pre\" id=\"rsync-cmds\">%s</pre><button class=\"copy-btn\" onclick=\"copyRsync('rsync-cmds',this)\">Copy</button></div></div>\n",
			len(missing), cmds)
	}
	fmt.Fprintf(w, "<div class=\"summary\"><div class=\"stat\"><div class=\"n n-red\">%d</div><div class=\"l\">Missing</div></div><div class=\"stat\"><div class=\"n n-yellow\">%d</div><div class=\"l\">Size Mismatch</div></div><div class=\"stat\"><div class=\"n n-green\">%d</div><div class=\"l\">Hash Mismatch</div></div><div class=\"stat\"><div class=\"n\" style=\"color:var(--blue)\">%d</div><div class=\"l\">Fuzzy Match</div></div></div>\n",
		len(missing), len(sizeMismatch), len(hashMismatch), len(result.FuzzyMatch))
	fmt.Fprint(w, "</body></html>\n")
}

func missingTableOrEmpty(rows, _ string, _ bool) string {
	if rows == "" {
		return "<p class=\"empty\">None - all source files found in dest</p>"
	}
	return rows
}

func sizeTableOrEmpty(rows string) string {
	if rows == "" {
		return "<p class=\"empty\">None</p>"
	}
	return fmt.Sprintf("<table><tr><th>Name</th><th>Paths (src / dst)</th><th>Size</th></tr>%s</table>", rows)
}

func hashSection(rows string, count int) string {
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("<div class=\"section\"><h2>Hash Mismatch (same name+size, different content)<span class=\"count\">%d</span></h2><table><tr><th>Name</th><th>Paths (src / dst)</th></tr>%s</table></div>\n",
		count, rows)
}

// ---------- helpers ----------

func loadIndex(path string) *Index {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		fatal(fmt.Errorf("parsing %s: %w", path, err))
	}
	return &idx
}

func parseExts(s string) map[string]bool {
	if s == "" {
		m := make(map[string]bool, len(defaultExts))
		for _, e := range defaultExts {
			m[e] = true
		}
		return m
	}
	m := make(map[string]bool)
	for _, e := range strings.Split(s, ",") {
		e = strings.TrimSpace(strings.ToLower(strings.TrimPrefix(e, ".")))
		if e != "" {
			m[e] = true
		}
	}
	return m
}

// splitArgs separates flag args (starting with -) from positional args so that
// flags work in any position (e.g. `scan /path --output file.json`).
func splitArgs(args []string) (flags []string, pos []string) {
	i := 0
	for i < len(args) {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flags = append(flags, args[i])
			}
		} else {
			pos = append(pos, a)
		}
		i++
	}
	return
}

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func htmlEsc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&#34;")
	return s
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
