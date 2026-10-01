package updatecheck

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/ipc"
)

func TestNewerComparesSemVerNotStrings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		current string
		latest  string
		want    bool
	}{
		{"0.9.0", "0.9.1", true},
		{"0.9.0", "0.9.0", false},
		{"0.9.1", "0.9.0", false},
		{"0.9.9", "0.10.0", true},  // lexicographic comparison would say no
		{"0.10.0", "0.9.9", false}, // and yes here
		{"1.0.0-rc.1", "1.0.0", true},
		{"1.0.0", "1.0.0-rc.2", false},
		{"1.0.0-rc.1", "1.0.0-rc.2", true},
		{"1.0.0", "garbage", false},
	}
	for _, tc := range cases {
		info := buildinfo.Info{Version: tc.current, BuiltBy: "goreleaser"}
		if got := Newer(info, &ipc.ReleaseInfo{Version: tc.latest}); got != tc.want {
			t.Errorf("Newer(%s, %s) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
	if Newer(buildinfo.Info{Version: "0.1.0", BuiltBy: "goreleaser"}, nil) {
		t.Error("nil latest must not be newer")
	}
}

func TestDevelopmentBuildsAreNeverCompared(t *testing.T) {
	t.Parallel()
	for _, info := range []buildinfo.Info{
		{Version: buildinfo.DevelVersion, BuiltBy: "source"},
		{Version: "0.0.0-20260916010203-deadbeefcafe+dirty", BuiltBy: "source", Modified: true},
		{Version: "0.3.0", BuiltBy: "source"},
		{Version: "0.3.0", BuiltBy: "goreleaser", Modified: true},
	} {
		if Comparable(info) || Newer(info, &ipc.ReleaseInfo{Version: "9.9.9"}) {
			t.Errorf("%+v must not produce a notice", info)
		}
	}
	for _, info := range []buildinfo.Info{
		{Version: "0.3.0", BuiltBy: "goreleaser"},
		{Version: "0.3.0", BuiltBy: "go install"},
		{Version: "0.3.0-rc.1", BuiltBy: "goreleaser"},
	} {
		if !Comparable(info) {
			t.Errorf("%+v must be comparable", info)
		}
	}
	if Channel(buildinfo.Info{Version: "0.3.0-rc.1"}) != ChannelPrerelease || Channel(buildinfo.Info{Version: "0.3.0"}) != ChannelStable || Channel(buildinfo.Info{Version: "devel"}) != ChannelStable {
		t.Error("channel selection")
	}
}

func TestDetectInstallationMethod(t *testing.T) {
	t.Parallel()
	info := buildinfo.Info{Version: "0.9.0", BuiltBy: "goreleaser"}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	repo := "acme/tool-releases"
	if a := Detect("/anywhere/purlview", buildinfo.Info{Version: "0.9.0", BuiltBy: "go install"}, env(nil), repo); a.Method != "go-install" || !strings.HasPrefix(a.Command, "go install ") {
		t.Errorf("go install: %+v", a)
	}
	switch runtime.GOOS {
	case "darwin", "linux":
		if a := Detect("/opt/homebrew/Caskroom/purlview/0.9.0/purlview", info, env(nil), repo); a.Method != "homebrew" || a.Command != "brew upgrade --cask purlview" {
			t.Errorf("homebrew: %+v", a)
		}
		home := t.TempDir()
		if a := Detect(filepath.Join(home, ".local", "bin", "purlview"), info, env(map[string]string{"HOME": home}), repo); a.Method != "installer-script" || a.Command != installSh || a.Link != "" {
			t.Errorf("installer script: %+v", a)
		}
	case "windows":
		scoop := t.TempDir()
		if a := Detect(filepath.Join(scoop, "apps", "purlview", "current", "purlview.exe"), info, env(map[string]string{"SCOOP": scoop}), repo); a.Method != "scoop" {
			t.Errorf("scoop: %+v", a)
		}
		local := t.TempDir()
		if a := Detect(filepath.Join(local, "Microsoft", "WinGet", "Packages", "IdylLabs.Purlview_x", "purlview.exe"), info, env(map[string]string{"LOCALAPPDATA": local}), repo); a.Method != "winget" {
			t.Errorf("winget: %+v", a)
		}
		if a := Detect(filepath.Join(local, "Programs", "purlview", "purlview.exe"), info, env(map[string]string{"LOCALAPPDATA": local}), repo); a.Method != "installer-script" || a.Command != installPs1 {
			t.Errorf("installer script: %+v", a)
		}
	}
	a := Detect(filepath.Join(t.TempDir(), "purlview"), info, env(nil), repo)
	if a.Method != "unknown" || a.Link != "https://github.com/acme/tool-releases/releases/latest" || a.Command != "" {
		t.Errorf("unknown: %+v", a)
	}
}

func TestNoticeLogShowsOneVersionOncePerDay(t *testing.T) {
	t.Parallel()
	log := NoticeLog{Path: filepath.Join(t.TempDir(), "cache", "update-notice.json")}
	now := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	if !log.Due("0.9.1", now) {
		t.Fatal("the first notice is due")
	}
	if log.Due("0.9.1", now.Add(23*time.Hour)) {
		t.Fatal("the same version is not due again the same day")
	}
	if !log.Due("0.9.2", now.Add(time.Hour)) {
		t.Fatal("a newer version is due at once")
	}
	if !log.Due("0.9.2", now.Add(25*time.Hour+time.Minute)) {
		t.Fatal("a day later the version is due again")
	}
	// A file that cannot be written only means the notice may show again.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !(NoticeLog{Path: filepath.Join(blocked, "update-notice.json")}).Due("0.9.1", now) {
		t.Fatal("an unwritable log never hides a notice")
	}
}
