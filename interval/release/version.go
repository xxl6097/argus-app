// Package web — version probing + self-upgrade orchestration.
//
// The current binary doesn't know how to upgrade itself in-process
// (replacing /usr/bin/argus-app while running ETXTBSY's). Instead
// we shell out to the published install.sh: it stops the service,
// swaps the binary, and starts it again via procd. The handler
// returns immediately and the user is expected to refresh after
// ~60s. State machine:
//
//   POST /api/upgrade  → write /tmp/argus-upgrade.sh, spawn detached
//                         shell, return 200 to caller
//   detached shell     → wget install.sh, exec with VERSION=<tag>,
//                         install.sh does stop/swap/start
//   procd              → respawns argus-app with the new binary
//
// The upgrade script is detached via setsid so that it survives
// argus-app's own death during the upgrade.
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// VersionInfo is the build-stamped identity of this running argus-app.
// Populated via main.go's ldflags variables.
type VersionInfo struct {
	Version string `json:"version"` // e.g. "v0.1.13" or "dev"
	Commit  string `json:"commit"`  // short hash, may be empty
	Date    string `json:"date"`    // RFC3339, may be empty
}

// LatestRelease is the trimmed shape we surface to the dashboard.
// Mirrors the subset of GitHub's release JSON we actually use.
type LatestRelease struct {
	TagName    string    `json:"tag_name"`
	Name       string    `json:"name"`
	HTMLURL    string    `json:"html_url"`
	Body       string    `json:"body"`         // release notes (markdown)
	Prerelease bool      `json:"prerelease"`
	Draft      bool      `json:"draft"`
	FetchedAt  time.Time `json:"fetched_at"`
}

// VersionService probes GitHub for the latest release with a tiny
// in-memory cache so a busy dashboard doesn't burn through the
// (unauthenticated) GitHub API rate limit.
type VersionService struct {
	repo    string // e.g. "xxl6097/argus-app"
	mirrors []string

	mu       sync.Mutex
	cached   *LatestRelease
	cachedAt time.Time
	ttl      time.Duration
}

// NewVersionService — repo is "owner/name". mirrors is the list of
// GitHub-accelerator prefixes to try after a direct api.github.com
// connection times out (same list install.sh uses).
func NewVersionService(repo string) *VersionService {
	return &VersionService{
		repo: repo,
		mirrors: []string{
			"https://gh-proxy.com",
		},
		ttl: 30 * time.Minute,
	}
}

// Latest fetches the latest release from GitHub. force=true bypasses
// the in-memory cache (used when the user explicitly clicks "check
// for updates" on the dashboard).
func (vs *VersionService) Latest(ctx context.Context, force bool) (*LatestRelease, error) {
	vs.mu.Lock()
	if !force && vs.cached != nil && time.Since(vs.cachedAt) < vs.ttl {
		c := *vs.cached
		vs.mu.Unlock()
		return &c, nil
	}
	vs.mu.Unlock()

	api := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", vs.repo)
	candidates := []string{api}
	for _, m := range vs.mirrors {
		candidates = append(candidates, strings.TrimRight(m, "/")+"/"+api)
	}

	var lastErr error
	client := &http.Client{Timeout: 12 * time.Second}
	for _, u := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		// Pretend to be something other than the default Go client
		// to dodge a CDN rule we hit on gh-proxy.
		req.Header.Set("User-Agent", "argus-app version-check")
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("upstream %d", resp.StatusCode)
			continue
		}
		var rel LatestRelease
		if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
			resp.Body.Close()
			lastErr = err
			continue
		}
		resp.Body.Close()
		rel.FetchedAt = time.Now()
		vs.mu.Lock()
		c := rel
		vs.cached = &c
		vs.cachedAt = rel.FetchedAt
		vs.mu.Unlock()
		return &rel, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no candidates succeeded")
	}
	return nil, lastErr
}

// HasUpdate compares semver-ish tag strings. We treat the strings as
// dotted decimals after stripping a leading "v"; non-numeric tag
// segments fall back to lexical compare. Good enough for vX.Y.Z.
//
// Pre-release-ish tags (vX.Y.Z-rcN, vX.Y.Z-test) are treated as
// "older than the equivalent release tag", matching the SemVer 2.0
// spec — so vX.Y.Z-rc1 → vX.Y.Z reports has_update=true.
func HasUpdate(current, latest string) bool {
	if current == "" || current == "dev" || latest == "" {
		return false
	}
	c := strings.TrimPrefix(current, "v")
	l := strings.TrimPrefix(latest, "v")
	if c == l {
		return false
	}
	cBase, cHasPre := splitPreRelease(c)
	lBase, lHasPre := splitPreRelease(l)
	cmp := compareDottedDecimals(cBase, lBase)
	if cmp != 0 {
		return cmp < 0
	}
	// Same numeric base. SemVer rule: a pre-release version has lower
	// precedence than the same numeric version without a pre-release.
	if cHasPre && !lHasPre {
		return true
	}
	if !cHasPre && lHasPre {
		return false
	}
	return false
}

// splitPreRelease splits "1.2.3-rc1" into ("1.2.3", true).
func splitPreRelease(s string) (string, bool) {
	if i := strings.IndexByte(s, '-'); i >= 0 {
		return s[:i], true
	}
	return s, false
}

// compareDottedDecimals returns -1 / 0 / +1 for "a vs b" where each
// is a dot-separated numeric string. Non-numeric segments fall back
// to lexical compare on that segment.
func compareDottedDecimals(a, b string) int {
	ap := strings.Split(a, ".")
	bp := strings.Split(b, ".")
	for i := 0; i < len(ap) && i < len(bp); i++ {
		ai, aerr := atoiSafe(ap[i])
		bi, berr := atoiSafe(bp[i])
		if aerr == nil && berr == nil {
			if ai < bi {
				return -1
			}
			if ai > bi {
				return 1
			}
			continue
		}
		if ap[i] < bp[i] {
			return -1
		}
		if ap[i] > bp[i] {
			return 1
		}
	}
	if len(ap) < len(bp) {
		return -1
	}
	if len(ap) > len(bp) {
		return 1
	}
	return 0
}

func atoiSafe(s string) (int, error) {
	// Stop at first non-digit so "1-rc1" parses as 1 (we only use
	// this on the leading numeric portion of a segment).
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, errors.New("non-numeric")
	}
	n := 0
	for i := 0; i < end; i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

// TriggerUpgrade writes a detached bootstrap script that downloads
// the published install.sh and re-execs it under VERSION=<targetTag>.
// Returns immediately; the actual stop/swap/start happens in the
// background and the user is told to refresh the page.
//
// targetTag empty → install.sh resolves "latest" itself.
func TriggerUpgrade(targetTag string) error {
	// Prefer mirror by default — direct GitHub is often unreachable
	// from CN routers, and install.sh has its own mirror fallback
	// anyway, but we still need to fetch install.sh itself first.
	const installSrc = "https://gh-proxy.com/https://github.com/xxl6097/argus-app/releases/latest/download/install.sh"
	const tmpInstall = "/tmp/argus-install.sh"
	const tmpBoot = "/tmp/argus-upgrade.sh"
	const upgradeLog = "/tmp/argus-upgrade.log"

	var versionLine string
	if targetTag != "" {
		versionLine = "export VERSION=" + shellQuote(targetTag) + "\n"
	}
	script := "#!/bin/sh\n" +
		"set -eu\n" +
		"sleep 2\n" +
		"log=" + upgradeLog + "\n" +
		"exec >\"$log\" 2>&1\n" +
		"echo \"[$(date)] argus-app upgrade starting, target=${VERSION:-latest}\"\n" +
		versionLine +
		"if command -v wget >/dev/null 2>&1; then\n" +
		"  wget -q -O " + tmpInstall + " '" + installSrc + "'\n" +
		"elif command -v curl >/dev/null 2>&1; then\n" +
		"  curl -fsSL -o " + tmpInstall + " '" + installSrc + "'\n" +
		"else\n" +
		"  echo 'no wget/curl available'\n" +
		"  exit 1\n" +
		"fi\n" +
		"chmod 0755 " + tmpInstall + "\n" +
		"sh " + tmpInstall + "\n" +
		"echo \"[$(date)] argus-app upgrade complete\"\n"
	if err := os.WriteFile(tmpBoot, []byte(script), 0o755); err != nil {
		return fmt.Errorf("write upgrade boot script: %w", err)
	}
	cmd := exec.Command("sh", tmpBoot)
	// Detach: new session so it survives our exit, no inherited fds.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// shellQuote wraps s in single quotes with embedded quotes escaped.
// Used to safely embed a user-supplied tag into the bootstrap script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
