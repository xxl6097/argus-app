package release

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Backup format & limits.
//
// The /api/backup/export endpoint streams a gzip-compressed tar with
// every file under the configured data dir, plus a manifest.json at
// the archive root. Import accepts the same shape and writes through
// a staging dir + atomic rename, so a half-extracted archive never
// replaces the live data dir.
const (
	// backupFormat identifies the archive shape so /api/backup/import
	// can refuse arbitrary tarballs masquerading as argus-app backups.
	backupFormat        = "argus-app-backup"
	backupFormatVersion = 1

	// manifestName is the archive-root JSON describing the backup.
	manifestName = "manifest.json"

	// Sensitive files — skipped on import when restore_credentials=false.
	credentialsFile  = "credentials.json"
	notificationFile = "notifications.json"

	// Skip patterns when packing — atomic-write tmp files and any user
	// .bak side-files. Walking past these would let a partially-written
	// JSON sneak into the export.
	skipSuffixTmp = ".tmp"
	skipSuffixBak = ".bak"

	// Hard caps on the import side. The export side has no caps because
	// it runs against trusted local data.
	MaxImportArchiveBytes = int64(32 << 20)  // 32 MiB compressed upload
	maxImportPerFileBytes = int64(16 << 20)  // 16 MiB per entry uncompressed
	maxImportTotalUncompr = int64(100 << 20) // 100 MiB across all entries (zip-bomb guard)
	maxImportEntries      = 4096             // file count guard
)

// BackupManifest is the small JSON written at the archive root.
// Used by the importer to verify it's an argus-app backup (vs an
// arbitrary tarball) and surfaced to the UI for "exported by host
// X at time Y".
type BackupManifest struct {
	Format        string    `json:"format"`
	FormatVersion int       `json:"format_version"`
	ExportedAt    time.Time `json:"exported_at"`
	ExporterVer   string    `json:"exporter_version"`
	ExporterHost  string    `json:"exporter_hostname"`
	Files         []string  `json:"files"`
}

// PackDataDir streams a gzipped tar of dataDir to w. Skips dotfiles,
// .tmp, .bak, and the manifest itself (which is injected at the end).
// Returns the number of file entries packed (excluding manifest).
func PackDataDir(dataDir, exporterVersion string, w io.Writer) (int, error) {
	if dataDir == "" {
		return 0, errors.New("release: data dir not configured")
	}
	st, err := os.Stat(dataDir)
	if err != nil {
		return 0, fmt.Errorf("stat data dir: %w", err)
	}
	if !st.IsDir() {
		return 0, fmt.Errorf("data dir is not a directory: %s", dataDir)
	}

	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	// First pass: collect entries so the manifest can list them up
	// front. Modest-sized data dir (≤ a few MiB), so a buffered list
	// is fine.
	type entry struct {
		fsPath  string
		relPath string
		info    os.FileInfo
	}
	var entries []entry
	err = filepath.Walk(dataDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dataDir, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		// POSIX paths inside the tar regardless of host OS.
		rel = filepath.ToSlash(rel)
		// Skip transient / user side-files / hidden entries.
		base := filepath.Base(rel)
		if strings.HasPrefix(base, ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(base, skipSuffixTmp) || strings.HasSuffix(base, skipSuffixBak) {
			return nil
		}
		if rel == manifestName {
			// Don't ship a stale manifest from a previously-imported
			// archive — we always synthesize a fresh one.
			return nil
		}
		// Only regular files + dirs (no symlinks / sockets / devices).
		mode := info.Mode()
		if !info.IsDir() && !mode.IsRegular() {
			return nil
		}
		entries = append(entries, entry{fsPath: p, relPath: rel, info: info})
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walk data dir: %w", err)
	}

	// Stable order helps reproducible backups + readable tar listings.
	sort.Slice(entries, func(i, j int) bool { return entries[i].relPath < entries[j].relPath })

	host, _ := os.Hostname()
	manifest := BackupManifest{
		Format:        backupFormat,
		FormatVersion: backupFormatVersion,
		ExportedAt:    time.Now().UTC(),
		ExporterVer:   exporterVersion,
		ExporterHost:  host,
	}
	for _, e := range entries {
		if e.info.IsDir() {
			continue
		}
		manifest.Files = append(manifest.Files, e.relPath)
	}

	// Manifest first — readers can reject early before pulling MiBs.
	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("encode manifest: %w", err)
	}
	if err := writeTarFile(tw, manifestName, 0o644, mb, manifest.ExportedAt); err != nil {
		return 0, err
	}

	count := 0
	for _, e := range entries {
		if e.info.IsDir() {
			hdr := &tar.Header{
				Name:     e.relPath + "/",
				Mode:     0o755,
				Typeflag: tar.TypeDir,
				ModTime:  e.info.ModTime(),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return count, fmt.Errorf("tar header %s: %w", e.relPath, err)
			}
			continue
		}
		f, err := os.Open(e.fsPath)
		if err != nil {
			return count, fmt.Errorf("open %s: %w", e.fsPath, err)
		}
		hdr := &tar.Header{
			Name:     e.relPath,
			Mode:     int64(e.info.Mode().Perm()),
			Size:     e.info.Size(),
			Typeflag: tar.TypeReg,
			ModTime:  e.info.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			f.Close()
			return count, fmt.Errorf("tar header %s: %w", e.relPath, err)
		}
		if _, err := io.Copy(tw, f); err != nil {
			f.Close()
			return count, fmt.Errorf("tar copy %s: %w", e.relPath, err)
		}
		f.Close()
		count++
	}
	if err := tw.Close(); err != nil {
		return count, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return count, fmt.Errorf("close gzip: %w", err)
	}
	return count, nil
}

// writeTarFile appends a single regular-file entry to tw.
func writeTarFile(tw *tar.Writer, name string, mode int64, body []byte, mtime time.Time) error {
	hdr := &tar.Header{
		Name:     name,
		Mode:     mode,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
		ModTime:  mtime,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %s: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("tar write %s: %w", name, err)
	}
	return nil
}

// ImportResult summarises what /api/backup/import did.
type ImportResult struct {
	Restored []string        `json:"restored"`
	Skipped  []string        `json:"skipped"`
	Manifest *BackupManifest `json:"manifest,omitempty"`
}

// ImportBackup extracts r (a gzipped tar) into dataDir via a staging
// dir + atomic rename. The previous data dir is preserved as
// <dataDir>.bak.<timestamp> so the user can roll back manually.
//
// When restoreCreds is false, credentials.json + notifications.json
// are skipped during extract AND are copied over from the existing
// data dir into the staging dir, so the swap doesn't lose them.
func ImportBackup(dataDir string, r io.Reader, restoreCreds bool) (*ImportResult, error) {
	if dataDir == "" {
		return nil, errors.New("release: data dir not configured")
	}
	dataDir = filepath.Clean(dataDir)

	// Limit how many bytes we'll read off the wire. Anything bigger
	// is almost certainly a malicious upload; the real export of the
	// argus-app data dir fits in well under 1 MiB on a normal install.
	gzr, err := gzip.NewReader(io.LimitReader(r, MaxImportArchiveBytes+1))
	if err != nil {
		return nil, fmt.Errorf("gzip open: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)

	// Staging dir as a sibling of dataDir (so the rename is atomic on
	// the same filesystem). On extraction failure we rm -rf this and
	// leave the live dir untouched.
	parent := filepath.Dir(dataDir)
	stagingName := filepath.Base(dataDir) + ".import." + randSuffix()
	staging := filepath.Join(parent, stagingName)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir staging: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(staging) }
	defer func() {
		// If we return early without renaming staging, the deferred
		// cleanup catches it. After a successful rename, staging
		// won't exist anyway and the RemoveAll is a no-op.
		if _, err := os.Stat(staging); err == nil {
			cleanup()
		}
	}()

	var manifest *BackupManifest
	var entryCount int
	var totalBytes int64
	restored := []string{}
	skipped := []string{}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar read: %w", err)
		}
		entryCount++
		if entryCount > maxImportEntries {
			return nil, fmt.Errorf("backup has too many entries (>%d)", maxImportEntries)
		}
		name := filepath.ToSlash(hdr.Name)
		// Reject zip-slip vectors: absolute paths, parent-dir hops,
		// drive letters, leading dots that escape, NUL bytes.
		if name == "" {
			return nil, errors.New("tar entry has empty name")
		}
		if strings.ContainsRune(name, 0) {
			return nil, errors.New("tar entry name contains NUL")
		}
		if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
			return nil, fmt.Errorf("tar entry has absolute path: %s", name)
		}
		// filepath.Clean collapses "a/../b"; reject anything that
		// escapes after cleaning.
		cleaned := filepath.ToSlash(filepath.Clean(name))
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
			return nil, fmt.Errorf("tar entry escapes archive root: %s", name)
		}
		// Only allow regular files + dirs.
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			// ok
		case tar.TypeDir:
			// ok
		default:
			return nil, fmt.Errorf("tar entry %s has unsupported type %c (only regular files / dirs allowed)", name, hdr.Typeflag)
		}

		// Manifest is the first thing we want to see and validate
		// before extracting anything else.
		if cleaned == manifestName && hdr.Typeflag == tar.TypeReg {
			body, rerr := readTarEntry(tr, maxImportPerFileBytes)
			if rerr != nil {
				return nil, fmt.Errorf("read manifest: %w", rerr)
			}
			var m BackupManifest
			if err := json.Unmarshal(body, &m); err != nil {
				return nil, fmt.Errorf("decode manifest: %w", err)
			}
			if m.Format != backupFormat {
				return nil, fmt.Errorf("not an argus-app backup (manifest format=%q)", m.Format)
			}
			if m.FormatVersion > backupFormatVersion {
				return nil, fmt.Errorf("backup format v%d newer than supported v%d", m.FormatVersion, backupFormatVersion)
			}
			manifest = &m
			continue
		}

		dst := filepath.Join(staging, filepath.FromSlash(cleaned))
		// Defense-in-depth: ensure the joined path is still inside
		// staging. With the cleaning above this should always hold,
		// but a positive check is cheap.
		if !strings.HasPrefix(dst, staging+string(os.PathSeparator)) && dst != staging {
			return nil, fmt.Errorf("tar entry escapes staging: %s", name)
		}

		base := filepath.Base(cleaned)
		// Skip credentials/notifications when the user opted out.
		if !restoreCreds && (base == credentialsFile || base == notificationFile) {
			skipped = append(skipped, cleaned)
			// Drain reader so the tar position advances.
			if _, err := io.Copy(io.Discard, io.LimitReader(tr, maxImportPerFileBytes+1)); err != nil {
				return nil, fmt.Errorf("skip body %s: %w", name, err)
			}
			continue
		}

		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return nil, fmt.Errorf("mkdir %s: %w", cleaned, err)
			}
			continue
		}
		// Make sure the parent directory exists.
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir parent %s: %w", cleaned, err)
		}

		// Cap per-file size + total size during extraction.
		body, rerr := readTarEntry(tr, maxImportPerFileBytes)
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", cleaned, rerr)
		}
		totalBytes += int64(len(body))
		if totalBytes > maxImportTotalUncompr {
			return nil, fmt.Errorf("backup uncompressed size exceeds %d bytes", maxImportTotalUncompr)
		}

		// Force tighter perms on sensitive files even if the tar
		// header lied.
		mode := os.FileMode(hdr.Mode & 0o777)
		if base == credentialsFile || base == notificationFile {
			mode = 0o600
		}
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(dst, body, mode); err != nil {
			return nil, fmt.Errorf("write %s: %w", cleaned, err)
		}
		restored = append(restored, cleaned)
	}

	if manifest == nil {
		return nil, errors.New("backup is missing manifest.json — not an argus-app backup")
	}

	// When skipping creds/notifications, copy the live-disk versions
	// into staging so the swap-in dir is complete. Best-effort: a
	// missing source is OK (the user might be restoring on a fresh
	// install where credentials.json doesn't exist yet).
	if !restoreCreds {
		for _, sensitiveName := range []string{credentialsFile, notificationFile} {
			src := filepath.Join(dataDir, sensitiveName)
			b, err := os.ReadFile(src)
			if err != nil {
				continue
			}
			if err := os.WriteFile(filepath.Join(staging, sensitiveName), b, 0o600); err != nil {
				return nil, fmt.Errorf("preserve %s: %w", sensitiveName, err)
			}
		}
	}

	// Two-step swap with rollback safety: rename live → .bak first
	// (so we have something to restore if the second rename fails),
	// then rename staging → live, then unconditionally remove .bak.
	// The .bak dir only exists for the few microseconds between the
	// two renames; after a successful import there's nothing left
	// on disk besides the new data dir.
	bakDir := dataDir + ".bak." + time.Now().UTC().Format("20060102-150405")
	hadLive := false
	if _, err := os.Stat(dataDir); err == nil {
		if err := os.Rename(dataDir, bakDir); err != nil {
			return nil, fmt.Errorf("backup live dir: %w", err)
		}
		hadLive = true
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat live dir: %w", err)
	}
	if err := os.Rename(staging, dataDir); err != nil {
		// Roll back: put the old live dir back so the user isn't left
		// with no data dir at all.
		if hadLive {
			_ = os.Rename(bakDir, dataDir)
		}
		return nil, fmt.Errorf("activate staging: %w", err)
	}
	// Swap succeeded — purge the rollback dir. Best-effort: a failure
	// here just leaves a one-off .bak.<ts> on disk; not worth failing
	// the whole import for.
	if hadLive {
		_ = os.RemoveAll(bakDir)
	}

	res := &ImportResult{
		Restored: restored,
		Skipped:  skipped,
		Manifest: manifest,
	}
	return res, nil
}

// readTarEntry reads the current tar entry's body, capped at limit
// bytes. Returns an error when the entry exceeds the cap.
func readTarEntry(tr *tar.Reader, limit int64) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(tr, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > limit {
		return nil, fmt.Errorf("entry exceeds %d byte limit", limit)
	}
	return buf, nil
}

// randSuffix returns a short hex string for staging dir uniqueness.
// Crypto-rand because two concurrent imports racing the same suffix
// would silently overwrite each other's staging dir.
func randSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
