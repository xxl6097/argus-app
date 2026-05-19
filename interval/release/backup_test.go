package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackupRoundTrip packs a synthetic data dir, then imports the
// archive into a different empty dir, and verifies every file came
// back with the same content + manifest is valid.
func TestBackupRoundTrip(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "aliases.json"), `{"aa:bb:cc:dd:ee:ff":"phone"}`, 0o644)
	mustWrite(t, filepath.Join(src, "settings.json"), `{"work_start":"09:00"}`, 0o644)
	mustWrite(t, filepath.Join(src, "credentials.json"), `{"username":"admin","password_hash":"x"}`, 0o600)
	mustWrite(t, filepath.Join(src, "notifications.json"), `{"topic":"abc"}`, 0o600)
	if err := os.MkdirAll(filepath.Join(src, "history"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWrite(t, filepath.Join(src, "history", "AA_BB.json"), `{"events":[]}`, 0o644)

	// ignored: tmp + bak side-files
	mustWrite(t, filepath.Join(src, "settings.json.tmp"), `garbage`, 0o644)
	mustWrite(t, filepath.Join(src, "aliases.json.bak"), `garbage`, 0o644)

	var buf bytes.Buffer
	n, err := PackDataDir(src, "v9.9.9", &buf)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if n < 4 {
		t.Fatalf("expected ≥4 file entries, got %d", n)
	}

	// The archive must NOT include the .tmp / .bak names.
	names := tarNamesInGz(t, buf.Bytes())
	for _, n := range names {
		if strings.HasSuffix(n, ".tmp") || strings.HasSuffix(n, ".bak") {
			t.Errorf("archive should skip transient entry %q", n)
		}
	}
	if !sliceContains(names, "manifest.json") {
		t.Errorf("manifest.json missing from archive: %v", names)
	}
	if !sliceContains(names, "history/AA_BB.json") {
		t.Errorf("nested history file missing: %v", names)
	}

	// Import into a fresh dir.
	dst := t.TempDir() + "/data"
	res, err := ImportBackup(dst, &buf, true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Manifest == nil || res.Manifest.Format != backupFormat {
		t.Fatalf("bad manifest: %+v", res.Manifest)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("nothing should be skipped with restoreCreds=true, got %v", res.Skipped)
	}

	// File content survived the round trip.
	checkFile(t, filepath.Join(dst, "aliases.json"), `{"aa:bb:cc:dd:ee:ff":"phone"}`)
	checkFile(t, filepath.Join(dst, "credentials.json"), `{"username":"admin","password_hash":"x"}`)
	checkFile(t, filepath.Join(dst, "history", "AA_BB.json"), `{"events":[]}`)

	// Sensitive files must be 0600 even if the source wasn't.
	st, err := os.Stat(filepath.Join(dst, "credentials.json"))
	if err != nil {
		t.Fatalf("stat credentials: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials.json perm = %o, want 0600", perm)
	}
}

// TestBackupSkipCredentials verifies that when restoreCreds=false the
// importer doesn't overwrite the existing credentials/notifications
// files with the ones from the archive.
func TestBackupSkipCredentials(t *testing.T) {
	// Source: contains credentials with value "from-backup".
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "aliases.json"), `{"x":"1"}`, 0o644)
	mustWrite(t, filepath.Join(src, "credentials.json"), `{"username":"admin","password_hash":"from-backup"}`, 0o600)
	mustWrite(t, filepath.Join(src, "notifications.json"), `{"topic":"from-backup"}`, 0o600)

	var buf bytes.Buffer
	if _, err := PackDataDir(src, "test", &buf); err != nil {
		t.Fatalf("pack: %v", err)
	}

	// Live dir already has DIFFERENT credentials we want to preserve.
	live := t.TempDir() + "/data"
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatalf("mkdir live: %v", err)
	}
	mustWrite(t, filepath.Join(live, "credentials.json"), `{"username":"admin","password_hash":"from-live"}`, 0o600)
	mustWrite(t, filepath.Join(live, "notifications.json"), `{"topic":"from-live"}`, 0o600)
	mustWrite(t, filepath.Join(live, "aliases.json"), `{"x":"old"}`, 0o644)

	res, err := ImportBackup(live, &buf, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	// aliases.json should be replaced.
	checkFile(t, filepath.Join(live, "aliases.json"), `{"x":"1"}`)
	// credentials + notifications should be the LIVE values (preserved).
	checkFile(t, filepath.Join(live, "credentials.json"), `{"username":"admin","password_hash":"from-live"}`)
	checkFile(t, filepath.Join(live, "notifications.json"), `{"topic":"from-live"}`)

	// Skipped list should mention both names.
	skippedNames := strings.Join(res.Skipped, ",")
	if !strings.Contains(skippedNames, "credentials.json") || !strings.Contains(skippedNames, "notifications.json") {
		t.Errorf("expected credentials.json + notifications.json in skipped, got %v", res.Skipped)
	}
}

// TestBackupRejectZipSlip ensures the importer refuses tarballs whose
// entries try to escape the staging dir.
func TestBackupRejectZipSlip(t *testing.T) {
	dst := t.TempDir() + "/data"
	cases := []struct {
		name  string
		entry string
	}{
		{"absolute path", "/etc/passwd"},
		{"parent dot-dot", "../../../etc/passwd"},
		{"nested escape", "history/../../escape.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tar := buildEvilTar(t, c.entry)
			_, err := ImportBackup(dst, bytes.NewReader(tar), true)
			if err == nil {
				t.Fatalf("expected error rejecting %q, got nil", c.entry)
			}
			if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
				t.Errorf("data dir should NOT exist after rejected import (got stat err %v)", statErr)
			}
		})
	}
}

// TestBackupRejectMissingManifest ensures a tar without manifest.json
// is refused even if all paths are otherwise safe.
func TestBackupRejectMissingManifest(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	body := []byte(`{"x":"1"}`)
	if err := tw.WriteHeader(&tar.Header{
		Name: "aliases.json", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()
	gw.Close()

	dst := t.TempDir() + "/data"
	_, err := ImportBackup(dst, bytes.NewReader(buf.Bytes()), true)
	if err == nil {
		t.Fatalf("expected error for missing manifest.json")
	}
	if !strings.Contains(err.Error(), "manifest") {
		t.Errorf("error should mention manifest, got: %v", err)
	}
}

// TestBackupRejectWrongFormat ensures a manifest with the wrong
// "format" string is refused (so an arbitrary unrelated tar with a
// manifest.json doesn't pass).
func TestBackupRejectWrongFormat(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	body := []byte(`{"format":"someone-else","format_version":1}`)
	if err := tw.WriteHeader(&tar.Header{
		Name: "manifest.json", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()
	gw.Close()

	dst := t.TempDir() + "/data"
	_, err := ImportBackup(dst, bytes.NewReader(buf.Bytes()), true)
	if err == nil {
		t.Fatalf("expected error for wrong format")
	}
	if !strings.Contains(err.Error(), "argus-app backup") {
		t.Errorf("error should mention format mismatch, got: %v", err)
	}
}

// --- helpers ---

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func checkFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("file %s mismatch:\n  want: %q\n  got:  %q", path, want, string(got))
	}
}

func tarNamesInGz(t *testing.T, body []byte) []string {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		names = append(names, hdr.Name)
		_, _ = io.Copy(io.Discard, tr)
	}
	return names
}

func sliceContains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// buildEvilTar emits a minimal valid manifest.json plus one entry
// with the given path. Used to exercise zip-slip rejection logic.
func buildEvilTar(t *testing.T, evilPath string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	man := []byte(`{"format":"argus-app-backup","format_version":1}`)
	tw.WriteHeader(&tar.Header{
		Name: "manifest.json", Mode: 0o644, Size: int64(len(man)), Typeflag: tar.TypeReg,
	})
	tw.Write(man)
	body := []byte(`pwn`)
	tw.WriteHeader(&tar.Header{
		Name: evilPath, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	})
	tw.Write(body)
	tw.Close()
	gw.Close()
	return buf.Bytes()
}
