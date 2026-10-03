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
	Path     string `json:"path"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Ext      string `json:"ext"`
	Modified int64  `json:"modified"`
	Hash     uint64 `json:"hash,omitempty"`
}

type Index struct {
	Root      string       `json:"root"`
	ScannedAt time.Time    `json:"scanned_at"`
	Files     []FileRecord `json:"files"`
}

type CompareResult struct {
	Missing      []FileRecord `json:"missing"`
	SizeMismatch []MismatchPair `json:"size_mismatch"`
	HashMismatch []MismatchPair `json:"hash_mismatch,omitempty"`
}

type MismatchPair struct {
	Source FileRecord `json:"source"`
	Dest   FileRecord `json:"dest"`
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
  compare      Compare two index files and report missing/mismatched files
  sync-check   Scan two paths directly and compare (both must be accessible)

EXAMPLES:
  # On transmission server (or mount the disk):
  file-syncer-cmp scan /mnt/disk1 --output disk1.json

  # On NAS (or mount the NAS share):
  file-syncer-cmp scan /mnt/nas --output nas.json

  # Compare the two indexes:
  file-syncer-cmp compare disk1.json nas.json

  # Or if both paths are accessible at once:
  file-syncer-cmp sync-check /mnt/disk1 /mnt/nas

  # With hash verification (slower but more accurate):
  file-syncer-cmp scan /mnt/disk1 --output disk1.json --hash
  file-syncer-cmp compare disk1.json nas.json --hash

  # Custom extensions:
  file-syncer-cmp scan /mnt/disk1 --ext mp3,flac,mkv,mp4

  # HTML report:
  file-syncer-cmp compare disk1.json nas.json --format html > report.html
`)
}

// ---------- scan ----------

func cmdScan(args []string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	output := fs.String("output", "", "output JSON file (default: stdout)")
	extList := fs.String("ext", "", "comma-separated extensions (default: media files)")
	doHash := fs.Bool("hash", false, "compute partial file hash (slower)")
	workers := fs.Int("workers", 8, "parallel scan workers")

	// Separate positional args from flags so flags work in any position.
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

	flagArgs, posArgs := splitArgs(args)
	fs.Parse(flagArgs)

	if len(posArgs) < 2 {
		fmt.Fprintln(os.Stderr, "usage: compare <source.json> <dest.json> [flags]")
		os.Exit(1)
	}

	src := loadIndex(posArgs[0])
	dst := loadIndex(posArgs[1])

	result := compare(src, dst, *doHash)
	printReport(result, src, dst, *format)
}

// ---------- sync-check ----------

func cmdSyncCheck(args []string) {
	fs := flag.NewFlagSet("sync-check", flag.ExitOnError)
	extList := fs.String("ext", "", "comma-separated extensions")
	doHash := fs.Bool("hash", false, "compute and compare hashes")
	format := fs.String("format", "text", "output format: text, json, html")
	workers := fs.Int("workers", 8, "parallel scan workers")

	flagArgs, posArgs := splitArgs(args)
	fs.Parse(flagArgs)

	if len(posArgs) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sync-check <source-path> <dest-path> [flags]")
		os.Exit(1)
	}

	exts := parseExts(*extList)

	fmt.Fprintf(os.Stderr, "scanning source: %s\n", posArgs[0])
	src, err := scan(posArgs[0], exts, *doHash, *workers)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "found %d files in source\n", len(src.Files))

	fmt.Fprintf(os.Stderr, "scanning dest: %s\n", posArgs[1])
	dst, err := scan(posArgs[1], exts, *doHash, *workers)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "found %d files in dest\n", len(dst.Files))

	result := compare(src, dst, *doHash)
	printReport(result, src, dst, *format)
}

// ---------- scan implementation ----------

func scan(root string, exts map[string]bool, doHash bool, workers int) (*Index, error) {
	type job struct{ path string; info os.FileInfo }

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

	// head
	n, _ := io.ReadFull(f, buf)
	h.Write(buf[:n])

	// tail (only if file is large enough)
	if info.Size() > chunk*2 {
		f.Seek(-chunk, io.SeekEnd)
		n, _ = io.ReadFull(f, buf)
		h.Write(buf[:n])
	}

	// include size in hash to distinguish differently-sized files with same head/tail
	var sizeBuf [8]byte
	for i := 0; i < 8; i++ {
		sizeBuf[i] = byte(info.Size() >> (i * 8))
	}
	h.Write(sizeBuf[:])

	return h.Sum64()
}

// ---------- compare implementation ----------

func compare(src, dst *Index, useHash bool) *CompareResult {
	// Build dest lookup: name -> []FileRecord (multiple files may share a name)
	byName := make(map[string][]FileRecord, len(dst.Files))
	for _, f := range dst.Files {
		key := strings.ToLower(f.Name)
		byName[key] = append(byName[key], f)
	}

	result := &CompareResult{}

	for _, sf := range src.Files {
		key := strings.ToLower(sf.Name)
		candidates, found := byName[key]
		if !found {
			result.Missing = append(result.Missing, sf)
			continue
		}

		// Find exact size match
		var sizeMatch *FileRecord
		for i := range candidates {
			if candidates[i].Size == sf.Size {
				sizeMatch = &candidates[i]
				break
			}
		}

		if sizeMatch == nil {
			// Name matches but no size match - report best candidate
			result.SizeMismatch = append(result.SizeMismatch, MismatchPair{
				Source: sf,
				Dest:   candidates[0],
			})
			continue
		}

		// Size matches - optionally check hash
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

	fmt.Fprintf(w, "\nFILE SYNC REPORT\n%s\n", sep)
	fmt.Fprintf(w, "Source:  %s (scanned %s, %d files)\n", src.Root, src.ScannedAt.Format("2006-01-02 15:04"), len(src.Files))
	fmt.Fprintf(w, "Dest:    %s (scanned %s, %d files)\n", dst.Root, dst.ScannedAt.Format("2006-01-02 15:04"), len(dst.Files))
	fmt.Fprintf(w, "%s\n\n", sep)

	if len(result.Missing) == 0 && len(result.SizeMismatch) == 0 && len(result.HashMismatch) == 0 {
		fmt.Fprintln(w, "ALL FILES PRESENT - no issues found")
		return
	}

	if len(result.Missing) > 0 {
		fmt.Fprintf(w, "MISSING FILES (%d) - present in source, not found in dest:\n%s\n", len(result.Missing), sep)
		for _, f := range result.Missing {
			fmt.Fprintf(w, "  [MISSING] %s  (%s)\n", f.Path, humanSize(f.Size))
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

	fmt.Fprintf(w, "%s\nSUMMARY: %d missing, %d size-mismatch, %d hash-mismatch\n",
		sep, len(result.Missing), len(result.SizeMismatch), len(result.HashMismatch))
}

func printHTML(result *CompareResult, src, dst *Index) {
	missing := result.Missing
	sizeMismatch := result.SizeMismatch
	hashMismatch := result.HashMismatch

	var missingBuf, sizeBuf, hashBuf strings.Builder
	for _, f := range missing {
		fmt.Fprintf(&missingBuf, `<tr><td class="path">%s</td><td class="ext">%s</td><td class="size">%s</td></tr>`,
			htmlEsc(f.Path), htmlEsc(f.Ext), humanSize(f.Size))
	}
	for _, p := range sizeMismatch {
		fmt.Fprintf(&sizeBuf, `<tr><td class="name">%s</td><td class="path">%s<br><span class="dst-path">%s</span></td><td class="size">%s → %s</td></tr>`,
			htmlEsc(p.Source.Name), htmlEsc(p.Source.Path), htmlEsc(p.Dest.Path),
			humanSize(p.Source.Size), humanSize(p.Dest.Size))
	}
	for _, p := range hashMismatch {
		fmt.Fprintf(&hashBuf, `<tr><td class="name">%s</td><td class="path">%s<br><span class="dst-path">%s</span></td></tr>`,
			htmlEsc(p.Source.Name), htmlEsc(p.Source.Path), htmlEsc(p.Dest.Path))
	}
	missingRows := missingBuf.String()
	sizeRows := sizeBuf.String()
	hashRows := hashBuf.String()

	status := "ok"
	statusText := "All files present"
	if len(missing)+len(sizeMismatch)+len(hashMismatch) > 0 {
		status = "warn"
		statusText = fmt.Sprintf("%d missing &middot; %d size-mismatch &middot; %d hash-mismatch",
			len(missing), len(sizeMismatch), len(hashMismatch))
	}

	fmt.Printf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>File Sync Report</title>
<style>
  :root { --bg:#0f1117; --surface:#1a1d27; --border:#2d3148; --text:#e2e8f0; --muted:#64748b; --red:#f87171; --yellow:#fbbf24; --green:#4ade80; --blue:#60a5fa; }
  * { box-sizing:border-box; margin:0; padding:0 }
  body { background:var(--bg); color:var(--text); font:14px/1.5 ui-monospace,monospace; padding:24px }
  h1 { font-size:18px; font-weight:600; margin-bottom:4px }
  .meta { color:var(--muted); font-size:12px; margin-bottom:24px }
  .status-ok { color:var(--green) }
  .status-warn { color:var(--red) }
  .section { margin-bottom:32px }
  .section h2 { font-size:13px; font-weight:600; text-transform:uppercase; letter-spacing:.08em; color:var(--muted); margin-bottom:8px; padding-bottom:6px; border-bottom:1px solid var(--border) }
  .section h2 .count { color:var(--red); margin-left:8px }
  table { width:100%%; border-collapse:collapse }
  th { text-align:left; font-size:11px; text-transform:uppercase; letter-spacing:.06em; color:var(--muted); padding:6px 10px; border-bottom:1px solid var(--border) }
  td { padding:7px 10px; border-bottom:1px solid var(--border); word-break:break-all }
  tr:hover td { background:var(--surface) }
  .path { color:var(--blue) }
  .dst-path { color:var(--muted); font-size:12px }
  .ext { color:var(--yellow); width:60px }
  .size { color:var(--muted); white-space:nowrap; width:100px }
  .name { color:var(--text); width:280px }
  .empty { color:var(--green); padding:10px }
  .summary { display:flex; gap:24px; margin-top:24px; padding-top:16px; border-top:1px solid var(--border) }
  .stat { text-align:center }
  .stat .n { font-size:28px; font-weight:700 }
  .stat .l { font-size:11px; color:var(--muted); text-transform:uppercase }
  .n-red { color:var(--red) }
  .n-yellow { color:var(--yellow) }
  .n-green { color:var(--green) }
</style>
</head>
<body>
<h1>File Sync Report</h1>
<div class="meta">
  Source: <b>%s</b> &middot; %d files &middot; scanned %s<br>
  Dest: <b>%s</b> &middot; %d files &middot; scanned %s<br>
  Status: <span class="status-%s">%s</span>
</div>

<div class="section">
  <h2>Missing Files<span class="count">%d</span></h2>
  %s
</div>

<div class="section">
  <h2>Size Mismatch<span class="count">%d</span></h2>
  %s
</div>

%s

<div class="summary">
  <div class="stat"><div class="n n-red">%d</div><div class="l">Missing</div></div>
  <div class="stat"><div class="n n-yellow">%d</div><div class="l">Size Mismatch</div></div>
  <div class="stat"><div class="n n-green">%d</div><div class="l">Hash Mismatch</div></div>
</div>
</body>
</html>`,
		htmlEsc(src.Root), len(src.Files), src.ScannedAt.Format("2006-01-02 15:04"),
		htmlEsc(dst.Root), len(dst.Files), dst.ScannedAt.Format("2006-01-02 15:04"),
		status, statusText,
		len(missing),
		missingTableOrEmpty(missingRows, "Path", "Ext", "Size"),
		len(sizeMismatch),
		sizeTableOrEmpty(sizeRows),
		hashSection(hashRows, len(hashMismatch)),
		len(missing), len(sizeMismatch), len(hashMismatch),
	)
}

func missingTableOrEmpty(rows, c1, c2, c3 string) string {
	if rows == "" {
		return `<p class="empty">None - all source files found in dest</p>`
	}
	return fmt.Sprintf(`<table><tr><th>%s</th><th>%s</th><th>%s</th></tr>%s</table>`, c1, c2, c3, rows)
}

func sizeTableOrEmpty(rows string) string {
	if rows == "" {
		return `<p class="empty">None</p>`
	}
	return fmt.Sprintf(`<table><tr><th>Name</th><th>Paths (src / dst)</th><th>Size</th></tr>%s</table>`, rows)
}

func hashSection(rows string, count int) string {
	if count == 0 {
		return ""
	}
	return fmt.Sprintf(`<div class="section">
  <h2>Hash Mismatch (same name+size, different content)<span class="count">%d</span></h2>
  <table><tr><th>Name</th><th>Paths (src / dst)</th></tr>%s</table>
</div>`, count, rows)
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

// splitArgs separates flag args (starting with -) from positional args so that
// flags work in any position (e.g. `scan /path --output file.json`).
func splitArgs(args []string) (flags []string, pos []string) {
	i := 0
	for i < len(args) {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			// If the flag looks like --key (no =value), consume next arg as value
			// unless next arg also starts with -.
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

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
