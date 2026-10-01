package updatecheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/ipc"
	"github.com/idyl-labs/purlview/internal/semver"
)

// Channel selects the release channel for a build: a prerelease build
// follows prereleases, everything else follows stable releases.
func Channel(info buildinfo.Info) string {
	if v, err := semver.Parse(info.Version); err == nil && v.IsPrerelease() {
		return ChannelPrerelease
	}
	return ChannelStable
}

// Comparable reports whether the running build can be compared with
// published releases. Builds from a checkout ("source", pseudo-versions,
// modified trees) and "devel" have no release to compare against, so they
// never produce a notice.
func Comparable(info buildinfo.Info) bool {
	if info.Version == buildinfo.DevelVersion || info.Modified || info.BuiltBy == "source" {
		return false
	}
	_, err := semver.Parse(info.Version)
	return err == nil
}

// Newer reports whether latest is newer than the running build.
func Newer(info buildinfo.Info, latest *ipc.ReleaseInfo) bool {
	if latest == nil || !Comparable(info) {
		return false
	}
	cur, err := semver.Parse(info.Version)
	if err != nil {
		return false
	}
	l, err := semver.Parse(latest.Version)
	if err != nil {
		return false
	}
	return semver.Compare(l, cur) > 0
}

// Advice is how this copy of Purlview is updated. The CLI's output package
// renders it as the update notice.
type Advice struct {
	// Method is a short name: homebrew, scoop, winget, deb, rpm,
	// installer-script, go-install or unknown.
	Method string
	// Command updates this copy the way it was installed, when one command
	// does; it is the command docs/installation.md gives for the route.
	Command string
	// Link is the release page, for routes that start with a download (deb,
	// rpm) and for a copy whose route is unknown.
	Link string
}

// Installer script commands, as in docs/installation.md.
const (
	installSh  = "curl -fsSLo install.sh https://purlview.com/install.sh && sh install.sh"
	installPs1 = "Invoke-WebRequest https://purlview.com/install.ps1 -OutFile install.ps1; powershell -ExecutionPolicy Bypass -File install.ps1"
)

// Detect works out how this executable was installed from its resolved
// path and build identity. It reads no metadata from the release service.
func Detect(exe string, info buildinfo.Info, getenv func(string) string, repository string) Advice {
	releasePage := "https://github.com/" + repository + "/releases/latest"
	if info.BuiltBy == "go install" {
		return Advice{Method: "go-install", Command: "go install github.com/idyl-labs/purlview/cmd/purlview@latest"}
	}
	resolved := exe
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		resolved = r
	}
	lower := strings.ToLower(filepath.ToSlash(resolved))
	switch runtime.GOOS {
	case "darwin", "linux":
		if strings.Contains(lower, "/caskroom/") || strings.Contains(lower, "/cellar/") {
			return Advice{Method: "homebrew", Command: "brew upgrade --cask purlview"}
		}
	case "windows":
		if scoop := getenv("SCOOP"); scoop != "" && hasPrefixFold(resolved, scoop) {
			return Advice{Method: "scoop", Command: "scoop update purlview"}
		}
		if strings.Contains(lower, "/scoop/apps/purlview/") {
			return Advice{Method: "scoop", Command: "scoop update purlview"}
		}
		if strings.Contains(lower, "/microsoft/winget/") {
			return Advice{Method: "winget", Command: "winget upgrade IdylLabs.Purlview"}
		}
		if local := getenv("LOCALAPPDATA"); local != "" && hasPrefixFold(resolved, filepath.Join(local, "Programs", "purlview")) {
			return Advice{Method: "installer-script", Command: installPs1}
		}
	}
	if runtime.GOOS == "linux" && resolved == "/usr/bin/purlview" {
		if _, err := os.Stat("/var/lib/dpkg/info/purlview.list"); err == nil {
			return Advice{Method: "deb", Link: releasePage}
		}
		if _, err := os.Stat("/var/lib/rpm"); err == nil {
			return Advice{Method: "rpm", Link: releasePage}
		}
	}
	if runtime.GOOS != "windows" {
		home := getenv("HOME")
		if home != "" && resolved == filepath.Join(home, ".local", "bin", "purlview") {
			return Advice{Method: "installer-script", Command: installSh}
		}
	}
	return Advice{Method: "unknown", Link: releasePage}
}

func hasPrefixFold(path, prefix string) bool {
	p := strings.ToLower(filepath.Clean(path))
	q := strings.ToLower(filepath.Clean(prefix))
	return p == q || strings.HasPrefix(p, q+string(filepath.Separator))
}

// NoticeLog remembers the last update notice the CLI showed, so one version
// is announced at most once a day. Nothing depends on it: a file that cannot
// be read or written only means the notice may show again.
type NoticeLog struct {
	Path string
}

type noticeRecord struct {
	Version string    `json:"version"`
	ShownAt time.Time `json:"shown_at"`
}

// Due reports whether a notice for version may be shown now and, when it
// may, records that it was.
func (l NoticeLog) Due(version string, now time.Time) bool {
	var last noticeRecord
	if data, err := os.ReadFile(l.Path); err == nil && json.Unmarshal(data, &last) == nil {
		if since := now.Sub(last.ShownAt); last.Version == version && since >= 0 && since < 24*time.Hour {
			return false
		}
	}
	if data, err := json.Marshal(noticeRecord{Version: version, ShownAt: now.UTC()}); err == nil {
		if os.MkdirAll(filepath.Dir(l.Path), 0o700) == nil {
			_ = os.WriteFile(l.Path, data, 0o600)
		}
	}
	return true
}
