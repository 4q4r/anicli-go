package cfbrowser

// Signed-manifest verification (PR15): every downloaded browser
// archive — free AND pro — is verified against the Ed25519-signed
// SHA256SUMS manifest before it may install. The public key is
// PINNED in the binary (no configuration, environment or API data
// can replace or disable it): a signature that does not verify is a
// BinaryVerificationError and nothing installs. The free channel
// tries manifests in order from two origins — the download base
// mirror and the GitHub release line — while the pro channel probes
// the distinct pro release line ({base}/releases/pro/…, no GitHub
// mirror); an origin counts as available only when BOTH SHA256SUMS
// and SHA256SUMS.sig fetch (HTTP-level absence at every origin is
// unavailability, NOT a verification failure — callers apply their
// channel policy: free falls back to the API digest field, pro
// fails loud).

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// PinnedManifestPubKeyB64 is the pinned Ed25519 public key verifying
// SHA256SUMS.sig files (base64, 32 raw bytes).
const PinnedManifestPubKeyB64 = "MKFKwIhUcKWq5xTuNA0Ovg99njcDEcEJvmWYYhApvaU="

// pinnedManifestPubKey decodes once at startup; a corrupt constant
// is a build defect, not a runtime condition.
var pinnedManifestPubKey = mustDecodePinnedManifestKey()

// manifestPublicKey is the key the install paths verify against.
// It defaults to the pinned key and has NO user-facing override
// (no env, no config, no API field can touch it); the var exists
// purely as the same-package test seam (the repo's afterFunc
// pattern) so end-to-end install tests can sign fixtures.
var manifestPublicKey = pinnedManifestPubKey

// manifestFetchTimeout bounds one SUMS/sig HTTP fetch.
const manifestFetchTimeout = 15 * time.Second

// BinaryVerificationError reports a downloaded archive that failed
// signed-manifest verification. It is terminal: the bytes are
// discarded and (on the pro channel) NEVER downgraded to an
// unsigned or free fetch.
type BinaryVerificationError struct {
	// Archive is the archive file name that failed verification.
	Archive string
	// Detail names the failed verification step.
	Detail string
}

// Error implements error.
func (e *BinaryVerificationError) Error() string {
	return fmt.Sprintf("cfbrowser: binary verification failed for %s: %s — archive discarded",
		e.Archive, e.Detail)
}

// mustDecodePinnedManifestKey decodes the pinned key at startup.
func mustDecodePinnedManifestKey() ed25519.PublicKey {
	raw, err := base64.StdEncoding.DecodeString(PinnedManifestPubKeyB64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		panic(fmt.Sprintf("cfbrowser: pinned manifest public key is corrupt (%d bytes, err=%v)", len(raw), err))
	}
	return ed25519.PublicKey(raw)
}

// manifestOrigin is one (SHA256SUMS, SHA256SUMS.sig) URL pair.
type manifestOrigin struct {
	sumsURL string
	sigURL  string
}

// manifestOrigins lists the manifest fetch origins for a channel.
//
// Free channel (upstream _fetch_signed_manifest parity):
//  1. the download-base mirror: {base}/chromium-v{version}/…
//  2. the GitHub release line:
//     github.com/CloakHQ/cloakbrowser/releases/download/chromium-v{version}/…
//     (passed through the $CLOAKBROWSER_DOWNLOAD_URL rewrite at
//     fetch time — a no-op when the env is unset).
//
// Pro channel (upstream _verify_pro_download parity, download.py:584):
// ONE origin on the DISTINCT pro release line —
// {base}/releases/pro/chromium-v{version}/… — pro archives have no
// GitHub free mirror.
//
// Deliberate divergence from upstream (documented for reviewers):
// upstream disables the Pro channel entirely when
// CLOAKBROWSER_DOWNLOAD_URL is set (download.py drops the license key
// under a custom download base, degrading to a skippable same-origin
// checksum). This port keeps the Pro channel live through the mirror
// AND keeps the pinned-key signed-manifest verification mandatory on
// every origin — a strictly stronger posture: the license unlocks
// the channel, never the checks.
func manifestOrigins(downloadBase, version, channel string) []manifestOrigin {
	if channel == channelPro {
		path := "/releases/pro/" + tagPrefix + version + "/" + sumsAssetName
		base := strings.TrimSuffix(downloadBase, "/")
		return []manifestOrigin{{sumsURL: base + path, sigURL: base + path + ".sig"}}
	}
	tag := tagPrefix + version
	path := "/" + tag + "/" + sumsAssetName
	base := strings.TrimSuffix(downloadBase, "/")
	primary := manifestOrigin{sumsURL: base + path, sigURL: base + path + ".sig"}
	github := manifestOrigin{
		sumsURL: "https://github.com/" + githubRepo + "/releases/download" + path,
		sigURL:  "https://github.com/" + githubRepo + "/releases/download" + path + ".sig",
	}
	return []manifestOrigin{primary, github}
}

// VerifyManifestsRequest scopes one archive verification.
type VerifyManifestsRequest struct {
	// DownloadBase is origin 1's base URL (default resolution: env
	// CLOAKBROWSER_DOWNLOAD_URL > https://cloakbrowser.dev).
	DownloadBase string
	// Version is the dotted browser version; the signed manifest's
	// version= line MUST declare exactly this version.
	Version string
	// Channel selects the manifest origin line: channelPro probes
	// the distinct pro release line; anything else (including the
	// channelFree zero value) probes the free origins.
	Channel string
	// ArchiveName is the archive file name the manifest must cover.
	ArchiveName string
	// ArchiveDigest is the downloaded archive's digest
	// ("sha256:<hex>", the download pipeline's format).
	ArchiveDigest string
}

// VerifyArchiveWithSignedManifests fetches SHA256SUMS+SHA256SUMS.sig
// from the origins of the request's channel, in order, and at the
// first origin that yields both files verifies the Ed25519 signature
// over the raw manifest bytes, the version= binding, the archive's
// line and the digest comparison.
//
//   - verified=true, err=nil: the archive is verified.
//   - verified=false, err=nil: manifests unavailable at every origin
//     (fetch-level failure — a caller policy decision, not a
//     verification failure).
//   - err != nil (*BinaryVerificationError): fetched manifests that
//     do not verify — bad signature, malformed base64, version
//     mismatch, missing archive line, digest mismatch.
func VerifyArchiveWithSignedManifests(ctx context.Context, req VerifyManifestsRequest, hc *http.Client) (bool, error) {
	if hc == nil {
		hc = &http.Client{Timeout: manifestFetchTimeout}
	}
	if err := validateVersion(req.Version); err != nil {
		return false, fmt.Errorf("cfbrowser: manifest version: %w", err)
	}
	for _, origin := range manifestOrigins(req.DownloadBase, req.Version, req.Channel) {
		sums, sig, err := fetchManifestPair(ctx, origin, hc)
		if err != nil {
			continue // origin unavailable: try the next one
		}
		return true, verifySignedManifest(sums, sig, manifestPublicKey, req.Version, req.ArchiveName, req.ArchiveDigest)
	}
	return false, nil
}

// fetchManifestPair fetches both files of one origin; any failure
// makes the whole origin unavailable.
func fetchManifestPair(ctx context.Context, origin manifestOrigin, hc *http.Client) (sums, sig []byte, err error) {
	sums, err = fetchManifestFile(ctx, origin.sumsURL, hc)
	if err != nil {
		return nil, nil, err
	}
	sig, err = fetchManifestFile(ctx, origin.sigURL, hc)
	if err != nil {
		return nil, nil, err
	}
	return sums, sig, nil
}

// maxManifestBytes bounds a manifest fetch (defensive: SUMS files
// are a few KB; a hostile endpoint must not stream forever).
const maxManifestBytes = 1 << 20

// fetchManifestFile GETs one manifest file. GitHub-shaped URLs pass
// through the $CLOAKBROWSER_DOWNLOAD_URL rewrite first (a no-op in
// production without the env).
func fetchManifestFile(ctx context.Context, rawURL string, hc *http.Client) ([]byte, error) {
	url := rawURL
	if strings.Contains(rawURL, "://github.com/") {
		rewritten, err := rewriteWithDownloadOverride(rawURL)
		if err != nil {
			return nil, err
		}
		url = rewritten
	}
	fctx, cancel := context.WithTimeout(ctx, manifestFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Drain a little so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("manifest %s: status %s", rawURL, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
}

// verifySignedManifest runs the full verification chain over one
// fetched origin: signature first, then the version binding, then
// the archive line, then the digest comparison.
func verifySignedManifest(sums, sigB64 []byte, pub ed25519.PublicKey, version, archiveName, archiveDigest string) error {
	// Upstream parity (download.py _verify_signature): surrounding
	// whitespace is stripped before the strict base64 decode — real
	// .sig files carry a trailing newline — while whitespace (or any
	// other junk) INSIDE the payload stays a verification failure.
	// Go's StdEncoding would silently ignore \r/\n anywhere, so inner
	// whitespace is rejected explicitly (validate=True semantics).
	sigStr := strings.TrimSpace(string(sigB64))
	if i := strings.IndexFunc(sigStr, unicode.IsSpace); i >= 0 {
		return &BinaryVerificationError{
			Archive: archiveName,
			Detail:  fmt.Sprintf("malformed base64 signature: whitespace inside payload at offset %d", i),
		}
	}
	sig, err := base64.StdEncoding.DecodeString(sigStr)
	if err != nil {
		return &BinaryVerificationError{
			Archive: archiveName,
			Detail:  fmt.Sprintf("malformed base64 signature: %v", err),
		}
	}
	if !ed25519.Verify(pub, sums, sig) {
		return &BinaryVerificationError{Archive: archiveName, Detail: "Ed25519 signature mismatch"}
	}
	// Version binding (upstream parity, both channels): the
	// signature proves "we made this manifest", not "this is the
	// version you requested" — without this check a mirror could
	// serve a genuinely-signed older release in place of the
	// requested one (forced downgrade). A manifest with no version=
	// line declares nothing and is refused the same way.
	if declared := manifestVersion(sums); declared != version {
		return &BinaryVerificationError{
			Archive: archiveName,
			Detail: fmt.Sprintf("version mismatch in signed SHA256SUMS: requested %s, manifest declares %q — refusing (possible downgrade)",
				version, declared),
		}
	}
	wantHex, ok := sumsDigestFor(sums, archiveName)
	if !ok {
		return &BinaryVerificationError{
			Archive: archiveName,
			Detail:  "signed manifest carries no line for this archive",
		}
	}
	gotHex := strings.TrimPrefix(strings.ToLower(archiveDigest), "sha256:")
	if gotHex != strings.ToLower(wantHex) {
		return &BinaryVerificationError{
			Archive: archiveName,
			Detail:  fmt.Sprintf("SHA-256 mismatch: downloaded %s, signed manifest says %s", gotHex, wantHex),
		}
	}
	return nil
}

// manifestVersion reads the "version=<v>" line from a manifest (""
// when absent) — mirrors upstream _parse_manifest_version: the line
// has no internal whitespace, so older two-field SHA256SUMS parsers
// ignore it.
func manifestVersion(sums []byte) string {
	for _, line := range strings.Split(string(sums), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "version="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// sumsDigestFor parses "<hex>  <name>" lines and returns the bare
// hex digest recorded for name.
func sumsDigestFor(sums []byte, name string) (string, bool) {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name && len(fields[0]) == 64 {
			return fields[0], true
		}
	}
	return "", false
}
