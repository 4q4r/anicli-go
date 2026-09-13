package cfbrowser

// Signed-manifest verification (PR15): every downloaded browser
// archive — free AND pro — is verified against the Ed25519-signed
// SHA256SUMS manifest before it may install. The public key is
// PINNED in the binary (no configuration, environment or API data
// can replace or disable it): a signature that does not verify is a
// BinaryVerificationError and nothing installs. Manifests are tried
// in order from two origins — the download base mirror and the
// GitHub release line; an origin counts as available only when BOTH
// SHA256SUMS and SHA256SUMS.sig fetch (HTTP-level absence at every
// origin is unavailability, NOT a verification failure — callers
// apply their channel policy: free falls back to the API digest
// field, pro fails loud).

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

// manifestOrigins lists the manifest fetch origins in order:
//  1. the download-base mirror: {base}/chromium-v{version}/…
//  2. the GitHub release line:
//     github.com/CloakHQ/cloakbrowser/releases/download/chromium-v{version}/…
//
// The GitHub URL is passed through the $CLOAKBROWSER_DOWNLOAD_URL
// rewrite at fetch time (unchanged when the env is unset).
func manifestOrigins(downloadBase, version string) []manifestOrigin {
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
	// Version is the dotted browser version.
	Version string
	// ArchiveName is the archive file name the manifest must cover.
	ArchiveName string
	// ArchiveDigest is the downloaded archive's digest
	// ("sha256:<hex>", the download pipeline's format).
	ArchiveDigest string
	// PublicKey verifies the signature; nil = the pinned key.
	PublicKey ed25519.PublicKey
}

// VerifyArchiveWithSignedManifests fetches SHA256SUMS+SHA256SUMS.sig
// from the origins in order and, at the first origin that yields
// both files, verifies the Ed25519 signature over the raw manifest
// bytes, locates the archive's line and compares digests.
//
//   - verified=true, err=nil: the archive is verified.
//   - verified=false, err=nil: manifests unavailable at every origin
//     (fetch-level failure — a caller policy decision, not a
//     verification failure).
//   - err != nil (*BinaryVerificationError): fetched manifests that
//     do not verify — bad signature, malformed base64, missing
//     archive line, digest mismatch.
func VerifyArchiveWithSignedManifests(ctx context.Context, req VerifyManifestsRequest, hc *http.Client) (bool, error) {
	if hc == nil {
		hc = &http.Client{Timeout: manifestFetchTimeout}
	}
	pub := req.PublicKey
	if pub == nil {
		pub = manifestPublicKey
	}
	if err := validateVersion(req.Version); err != nil {
		return false, fmt.Errorf("cfbrowser: manifest version: %w", err)
	}
	for _, origin := range manifestOrigins(req.DownloadBase, req.Version) {
		sums, sig, err := fetchManifestPair(ctx, origin, hc)
		if err != nil {
			continue // origin unavailable: try the next one
		}
		return true, verifySignedManifest(sums, sig, pub, req.ArchiveName, req.ArchiveDigest)
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
// fetched origin: signature first, then the archive line, then the
// digest comparison.
func verifySignedManifest(sums, sigB64 []byte, pub ed25519.PublicKey, archiveName, archiveDigest string) error {
	sig, err := base64.StdEncoding.DecodeString(string(sigB64))
	if err != nil {
		return &BinaryVerificationError{
			Archive: archiveName,
			Detail:  fmt.Sprintf("malformed base64 signature: %v", err),
		}
	}
	if !ed25519.Verify(pub, sums, sig) {
		return &BinaryVerificationError{Archive: archiveName, Detail: "Ed25519 signature mismatch"}
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
