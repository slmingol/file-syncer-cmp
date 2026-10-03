package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
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

  # Custom extensions:
  file-syncer-cmp scan /mnt/disk1 --ext mp3,flac,mkv,mp4

  # HTML report:
  file-syncer-cmp compare --dest nas.json disk1.json disk2.json --format html > report.html
`)
}

// ---------- scan ----------

func cmdScan(args []string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	output := fs.String("output", "", "output JSON file (default: stdout)")
	extList := fs.String("ext", "", "comma-separated extensions (default: media files)")
	doHash := fs.Bool("hash", false, "compute partial file hash (slower)")
	workers := fs.Int("workers", 8, "parallel scan workers")

	flagArgs, posArgs := splitArgs(args)
	fs.Parse(flagArgs)

	if len(posArgs) < 1 {
		fmt.Fprintln(os.Stderr, "usage: scan <path> [flags]")
		os.Exit(1)
	}

	root := posArgs[0]
	exts := parseExts(*extList)

	fmt.Fprintf(os.Stderr, "scanning %s ...\n", root)
	idx, err := scan(root, exts, *doHash, *workers)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "found %d files\n", len(idx.Files))

	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		fatal(err)
	}

	if *output == "" {
		os.Stdout.Write(data)
		fmt.Println()
	} else {
		if err := os.WriteFile(*output, data, 0644); err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *output)
	}
}

// ---------- compare ----------

func cmdCompare(args []string) {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	format := fs.String("format", "text", "output format: text, json, html")
	doHash := fs.Bool("hash", false, "also compare hashes (both indexes must have hashes)")
	doFuzzy := fs.Bool("fuzzy", false, "fuzzy name match: treat dest file as found if src name is substring of dest name")
	destFlag := fs.String("dest", "", "destination index (required when passing multiple sources)")

	flagArgs, posArgs := splitArgs(args)
	fs.Parse(flagArgs)

	var srcIndexes []*Index
	var dst *Index

	switch {
	case *destFlag != "":
		// --dest nas.json disk1.json disk2.json ...
		if len(posArgs) < 1 {
			fmt.Fprintln(os.Stderr, "usage: compare --dest <dest.json> <src1.json> [src2.json ...]")
			os.Exit(1)
		}
		dst = loadIndex(*destFlag)
		for _, p := range posArgs {
			srcIndexes = append(srcIndexes, loadIndex(p))
		}
	case len(posArgs) == 2:
		// legacy: compare src.json dst.json
		srcIndexes = []*Index{loadIndex(posArgs[0])}
		dst = loadIndex(posArgs[1])
	default:
		fmt.Fprintln(os.Stderr, "usage: compare <src.json> <dest.json>")
		fmt.Fprintln(os.Stderr, "       compare --dest <dest.json> <src1.json> [src2.json ...]")
		os.Exit(1)
	}

	src := mergeIndexes(srcIndexes)
	result := compare(src, dst, *doHash, *doFuzzy)
	printReport(result, src, dst, *format)
}

// ---------- sync-check ----------

func cmdSyncCheck(args []string) {
	fs := flag.NewFlagSet("sync-check", flag.ExitOnError)
	extList := fs.String("ext", "", "comma-separated extensions")
	doHash := fs.Bool("hash", false, "compute and compare hashes")
	doFuzzy := fs.Bool("fuzzy", false, "fuzzy name match: treat dest file as found if src name is substring of dest name")
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

	var srcIndexes []*Index
	for _, p := range srcPaths {
		fmt.Fprintf(os.Stderr, "scanning source: %s\n", p)
		idx, err := scan(p, exts, *doHash, *workers)
		if err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stderr, "found %d files\n", len(idx.Files))
		srcIndexes = append(srcIndexes, idx)
	}

	fmt.Fprintf(os.Stderr, "scanning dest: %s\n", destPath)
	dst, err := scan(destPath, exts, *doHash, *workers)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "found %d files in dest\n", len(dst.Files))

	src := mergeIndexes(srcIndexes)
	result := compare(src, dst, *doHash, *doFuzzy)
	printReport(result, src, dst, *format)
}

// ---------- scan implementation ----------

func scan(root string, exts map[string]bool, doHash bool, workers int) (*Index, error) {
	type job struct {
		path string
		info os.FileInfo
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

	go func() {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
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

	// For fuzzy: build a flat list of all dest files for substring scan.
	// Only populated when fuzzy mode is on.
	var allDest []FileRecord
	if fuzzy {
		allDest = dst.Files
	}

	result := &CompareResult{}

	for _, sf := range src.Files {
		key := strings.ToLower(sf.Name)
		candidates, found := byName[key]
		if !found {
			// Try fuzzy: src name is a substring of some dest name (or vice versa).
			if fuzzy {
				srcLow := strings.ToLower(sf.Name)
				var best *FileRecord
				for i := range allDest {
					dLow := strings.ToLower(allDest[i].Name)
					if strings.Contains(dLow, srcLow) || strings.Contains(srcLow, dLow) {
						best = &allDest[i]
						break
					}
				}
				if best != nil {
					result.FuzzyMatch = append(result.FuzzyMatch, FuzzyPair{
						Source:    sf,
						Dest:      *best,
						SizeMatch: sf.Size == best.Size,
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
		fmt.Fprintf(w, "FUZZY MATCH (%d) - src name substring of dest name (likely renamed):\n%s\n", len(result.FuzzyMatch), sep)
		for _, p := range result.FuzzyMatch {
			sizeTag := "size matches"
			if !p.SizeMatch {
				sizeTag = "SIZE DIFFERS"
			}
			fmt.Fprintf(w, "  [FUZZY/%s] %s\n", sizeTag, p.Source.Name)
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
	for _, f := range missing {
		if multiSrc {
			fmt.Fprintf(&missingBuf, "<tr><td class=\"path\">%s</td><td class=\"dst-path\">%s</td><td class=\"ext\">%s</td><td class=\"size\">%s</td></tr>",
				htmlEsc(f.Path), htmlEsc(f.SourceIndex), htmlEsc(f.Ext), humanSize(f.Size))
		} else {
			fmt.Fprintf(&missingBuf, "<tr><td class=\"path\">%s</td><td class=\"ext\">%s</td><td class=\"size\">%s</td></tr>",
				htmlEsc(f.Path), htmlEsc(f.Ext), humanSize(f.Size))
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
		fmt.Fprintf(&fuzzyBuf, "<tr><td class=\"name\">%s</td><td class=\"path\">%s<br><span class=\"dst-path\">%s</span></td><td class=\"size\">%s &rarr; %s</td><td>%s</td></tr>",
			htmlEsc(p.Source.Name), htmlEsc(p.Source.Path), htmlEsc(p.Dest.Path),
			humanSize(p.Source.Size), humanSize(p.Dest.Size), sizeTag)
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
	fmt.Fprint(w, "</style></head><body>\n")
	fmt.Fprint(w, "<h1>File Sync Report</h1>\n")
	fmt.Fprintf(w, "<div class=\"meta\">\n  Source: %s<br>\n  Dest: <b>%s</b> &middot; %d files &middot; scanned %s<br>\n  Status: <span class=\"status-%s\">%s</span>\n</div>\n",
		srcMeta, htmlEsc(dst.Root), len(dst.Files), dst.ScannedAt.Format("2006-01-02 15:04"), status, statusText)
	fmt.Fprintf(w, "<div class=\"section\"><h2>Missing Files<span class=\"count\">%d</span></h2>%s</div>\n",
		len(missing), missingTableOrEmpty(missingRows, missingHeader, multiSrc))
	fmt.Fprintf(w, "<div class=\"section\"><h2>Size Mismatch<span class=\"count\">%d</span></h2>%s</div>\n",
		len(sizeMismatch), sizeTableOrEmpty(sizeRows))
	fmt.Fprint(w, hashSection(hashRows, len(hashMismatch)))
	if fuzzyRows != "" {
		fmt.Fprintf(w, "<div class=\"section\"><h2>Fuzzy Match — likely renamed<span class=\"count\" style=\"color:var(--blue)\">%d</span></h2><p style=\"color:var(--muted);font-size:12px;margin-bottom:8px\">src name is substring of dest name (or vice versa) — file probably copied but renamed</p><table><tr><th>Name</th><th>Paths (src / dest)</th><th>Size</th><th>Size Check</th></tr>%s</table></div>\n",
			len(result.FuzzyMatch), fuzzyRows)
	}
	fmt.Fprintf(w, "<div class=\"summary\"><div class=\"stat\"><div class=\"n n-red\">%d</div><div class=\"l\">Missing</div></div><div class=\"stat\"><div class=\"n n-yellow\">%d</div><div class=\"l\">Size Mismatch</div></div><div class=\"stat\"><div class=\"n n-green\">%d</div><div class=\"l\">Hash Mismatch</div></div><div class=\"stat\"><div class=\"n\" style=\"color:var(--blue)\">%d</div><div class=\"l\">Fuzzy Match</div></div></div>\n",
		len(missing), len(sizeMismatch), len(hashMismatch), len(result.FuzzyMatch))
	fmt.Fprint(w, "</body></html>\n")
}

func missingTableOrEmpty(rows, pathHeader string, multiSrc bool) string {
	if rows == "" {
		return "<p class=\"empty\">None - all source files found in dest</p>"
	}
	if multiSrc {
		return fmt.Sprintf("<table><tr><th>%s</th><th>Source Disk</th><th>Ext</th><th>Size</th></tr>%s</table>", htmlEsc(pathHeader), rows)
	}
	return fmt.Sprintf("<table><tr><th>%s</th><th>Ext</th><th>Size</th></tr>%s</table>", htmlEsc(pathHeader), rows)
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
