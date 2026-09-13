package cfbrowser

import (
	"fmt"
	"strconv"
	"strings"
)

// versionPrefix is the CloakHQ release tag prefix ("chromium-v146.0…"
// → "146.0…"); dirPrefix mirrors it for cache directory names
// (chromium-146.0…, verified against the locally installed layout).
const (
	tagPrefix  = "chromium-v"
	dirPrefix  = "chromium-"
	minSegs    = 2 // at least major.minor
	proSuffix  = "-pro"
	maxVersion = 40 // defensive bound on segment count
)

// ParseVersionFromTag extracts the dotted version from a CloakHQ
// release tag. Pro-line tags (chromium-v151.0.7922.108.6-pro) yield
// their bare version — the channel distinction lives in release
// metadata, not the version ordering.
func ParseVersionFromTag(tag string) (string, error) {
	if !strings.HasPrefix(tag, tagPrefix) {
		return "", fmt.Errorf("cfbrowser: tag %q: missing %q prefix", tag, tagPrefix)
	}
	v := strings.TrimSuffix(strings.TrimPrefix(tag, tagPrefix), proSuffix)
	if err := validateVersion(v); err != nil {
		return "", err
	}
	return v, nil
}

// validateVersion rejects anything that is not dot-separated decimal
// segments (major.minor at minimum).
func validateVersion(v string) error {
	if v == "" {
		return fmt.Errorf("cfbrowser: empty version")
	}
	segs := strings.Split(v, ".")
	if len(segs) < minSegs || len(segs) > maxVersion {
		return fmt.Errorf("cfbrowser: version %q: want major.minor[.patch…]", v)
	}
	for _, s := range segs {
		if _, err := strconv.ParseUint(s, 10, 64); err != nil {
			return fmt.Errorf("cfbrowser: version %q: segment %q: %w", v, s, err)
		}
	}
	return nil
}

// CompareVersions orders two dotted versions numerically, segment by
// segment; when every shared segment is equal, the longer version
// sorts later (146 < 146.0 < 146.0.1) so cache-dir ordering is total.
// Returns -1, 0 or 1. Invalid segments compare as 0.
func CompareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	n := max(len(as), len(bs))
	for i := range n {
		av, bv := versionSegment(as, i), versionSegment(bs, i)
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	default:
		return 0
	}
}

// versionSegment parses segment i of segs, treating absent/invalid as 0.
func versionSegment(segs []string, i int) uint64 {
	if i >= len(segs) {
		return 0
	}
	v, err := strconv.ParseUint(segs[i], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// VersionDirName renders the cache directory name for a version
// (chromium-146.0.7680.177.5).
func VersionDirName(version string) string {
	return dirPrefix + version
}

// VersionFromDirName extracts the version from a cache directory name;
// ok is false for non-chromium directories and malformed versions.
func VersionFromDirName(dir string) (version string, ok bool) {
	if !strings.HasPrefix(dir, dirPrefix) {
		return "", false
	}
	v := strings.TrimPrefix(dir, dirPrefix)
	if v == "" {
		return "", false
	}
	for _, s := range strings.Split(v, ".") {
		if _, err := strconv.ParseUint(s, 10, 64); err != nil {
			return "", false
		}
	}
	return v, true
}
