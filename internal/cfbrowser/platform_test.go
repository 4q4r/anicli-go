package cfbrowser

import (
	"errors"
	"runtime"
	"testing"
)

func TestPlatformAssetFor(t *testing.T) {
	cases := []struct {
		name    string
		goos    string
		goarch  string
		asset   string
		archive archiveKind
		exec    string
		wantErr bool
	}{
		{"linux amd64", "linux", "amd64", "cloakbrowser-linux-x64.tar.gz", archiveTarGz, "chrome", false},
		{"linux arm64", "linux", "arm64", "cloakbrowser-linux-arm64.tar.gz", archiveTarGz, "chrome", false},
		{"windows amd64", "windows", "amd64", "cloakbrowser-windows-x64.zip", archiveZip, "chrome.exe", false},
		{"darwin arm64", "darwin", "arm64", "cloakbrowser-darwin-arm64.tar.gz", archiveTarGz, "Chromium.app/Contents/MacOS/Chromium", false},
		{"darwin amd64", "darwin", "amd64", "cloakbrowser-darwin-x64.tar.gz", archiveTarGz, "Chromium.app/Contents/MacOS/Chromium", false},
		{"linux 386 unsupported", "linux", "386", "", "", "", true},
		{"windows arm64 unsupported", "windows", "arm64", "", "", "", true},
		{"freebsd unsupported", "freebsd", "amd64", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := platformAssetFor(tc.goos, tc.goarch)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				var unsupported *UnsupportedPlatformError
				if !errors.As(err, &unsupported) {
					t.Fatalf("expected *UnsupportedPlatformError, got %T: %v", err, err)
				}
				if unsupported.GOOS != tc.goos || unsupported.GOARCH != tc.goarch {
					t.Fatalf("error carries %s/%s, want %s/%s", unsupported.GOOS, unsupported.GOARCH, tc.goos, tc.goarch)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Asset != tc.asset {
				t.Errorf("asset = %q, want %q", got.Asset, tc.asset)
			}
			if got.Archive != tc.archive {
				t.Errorf("archive = %v, want %v", got.Archive, tc.archive)
			}
			if got.ExecName != tc.exec {
				t.Errorf("exec = %q, want %q", got.ExecName, tc.exec)
			}
		})
	}
}

func TestCurrentPlatformAsset(t *testing.T) {
	got, err := CurrentPlatform()
	if err != nil {
		t.Fatalf("current platform must always resolve on a supported build: %v", err)
	}
	if got.Asset == "" || got.ExecName == "" {
		t.Fatalf("incomplete platform spec: %+v", got)
	}
	if runtime.GOOS == "linux" && got.ExecName != "chrome" {
		t.Errorf("linux exec = %q, want chrome", got.ExecName)
	}
}
