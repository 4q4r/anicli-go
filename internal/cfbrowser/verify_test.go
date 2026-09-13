package cfbrowser

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// manifestTestKey generates a fresh Ed25519 keypair for signing test
// manifests (the production path pins the real key).
func manifestTestKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// manifestBody renders a SHA256SUMS payload listing archive with its
// digest (bare hex, two-space separator — the upstream format).
func manifestBody(archive string, digestHex string) []byte {
	return []byte(digestHex + "  " + archive + "\n")
}

func digestHexOf(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}

// signManifest signs raw manifest bytes, returning base64 (the wire
// format of SHA256SUMS.sig).
func signManifest(priv ed25519.PrivateKey, raw []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw))
}

// manifestSrv serves SUMS/sig pairs at both origin shapes:
//
//	{base}/chromium-v{version}/SHA256SUMS[.sig]           (primary)
//	{base}/CloakHQ/cloakbrowser/releases/download/{tag}/… (github fallback, env-rewritten)
//
// origin selects which shape is served; hits counts requests.
type manifestSrv struct {
	srv     *httptest.Server
	hits    atomic.Int64
	sums    atomic.Value // []byte
	sig     atomic.Value // string
	primary bool
}

func newManifestSrv(t *testing.T, primary bool) *manifestSrv {
	t.Helper()
	m := &manifestSrv{primary: primary}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		isPrimary := strings.HasPrefix(r.URL.Path, "/chromium-v")
		if isPrimary != m.primary {
			http.NotFound(w, r)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+sumsAssetName+".sig"):
			sig, _ := m.sig.Load().(string)
			if sig == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(sig))
		case strings.HasSuffix(r.URL.Path, "/"+sumsAssetName):
			sums, _ := m.sums.Load().([]byte)
			if sums == nil {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(sums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *manifestSrv) serve(sums []byte, sig string) {
	m.sums.Store(sums)
	m.sig.Store(sig)
}

// verifyFixture wires two servers as the two manifest origins:
// primary (download base) and fallback (github shape via
// $CLOAKBROWSER_DOWNLOAD_URL rewrite).
type verifyFixture struct {
	primary, fallback *manifestSrv
	req               VerifyManifestsRequest
}

func newVerifyFixture(t *testing.T, pub ed25519.PublicKey) *verifyFixture {
	t.Helper()
	primary := newManifestSrv(t, true)
	fallback := newManifestSrv(t, false)
	t.Setenv(EnvDownloadURL, fallback.srv.URL)
	return &verifyFixture{
		primary:  primary,
		fallback: fallback,
		req: VerifyManifestsRequest{
			Version:       "146.0.7680.177.5",
			ArchiveName:   linuxX64Asset,
			ArchiveDigest: "sha256:" + strings.Repeat("ab", 32),
			PublicKey:     pub,
		},
	}
}

func TestVerifyManifestsValidSignature(t *testing.T) {
	pub, priv := manifestTestKey(t)
	fx := newVerifyFixture(t, pub)
	manifest := manifestBody(linuxX64Asset, strings.Repeat("ab", 32))
	fx.primary.serve(manifest, signManifest(priv, manifest))
	fx.req.DownloadBase = fx.primary.srv.URL

	verified, err := VerifyArchiveWithSignedManifests(context.Background(), fx.req, nil)
	if err != nil {
		t.Fatalf("valid manifest must verify: %v", err)
	}
	if !verified {
		t.Fatal("verified = false, want true")
	}
	if fx.fallback.hits.Load() != 0 {
		t.Errorf("primary origin served the manifests; fallback must stay untouched (hits=%d)", fx.fallback.hits.Load())
	}
}

func TestVerifyManifestsFallbackOrigin(t *testing.T) {
	pub, priv := manifestTestKey(t)
	fx := newVerifyFixture(t, pub)
	// Primary 404s everything; the github-shaped fallback serves.
	manifest := manifestBody(linuxX64Asset, strings.Repeat("ab", 32))
	fx.fallback.serve(manifest, signManifest(priv, manifest))
	fx.req.DownloadBase = fx.primary.srv.URL

	verified, err := VerifyArchiveWithSignedManifests(context.Background(), fx.req, nil)
	if err != nil {
		t.Fatalf("fallback origin must verify: %v", err)
	}
	if !verified {
		t.Fatal("verified = false, want true")
	}
}

func TestVerifyManifestsTamperedManifestFails(t *testing.T) {
	pub, priv := manifestTestKey(t)
	fx := newVerifyFixture(t, pub)
	manifest := manifestBody(linuxX64Asset, strings.Repeat("ab", 32))
	tampered := manifestBody(linuxX64Asset, strings.Repeat("cd", 32)) // digest swapped post-signing
	fx.primary.serve(tampered, signManifest(priv, manifest))
	fx.req.DownloadBase = fx.primary.srv.URL

	_, err := VerifyArchiveWithSignedManifests(context.Background(), fx.req, nil)
	var verification *BinaryVerificationError
	if !asVerifyErr(err, &verification) {
		t.Fatalf("tampered manifest must fail as BinaryVerificationError, got %v", err)
	}
}

func TestVerifyManifestsTamperedArchiveFails(t *testing.T) {
	pub, priv := manifestTestKey(t)
	fx := newVerifyFixture(t, pub)
	manifest := manifestBody(linuxX64Asset, strings.Repeat("ab", 32))
	fx.primary.serve(manifest, signManifest(priv, manifest))
	fx.req.ArchiveDigest = "sha256:" + strings.Repeat("ef", 32) // archive bytes differ
	fx.req.DownloadBase = fx.primary.srv.URL

	_, err := VerifyArchiveWithSignedManifests(context.Background(), fx.req, nil)
	var verification *BinaryVerificationError
	if !asVerifyErr(err, &verification) {
		t.Fatalf("digest mismatch must fail as BinaryVerificationError, got %v", err)
	}
}

func TestVerifyManifestsWrongKeyFails(t *testing.T) {
	_, signer := manifestTestKey(t)
	pub, _ := manifestTestKey(t) // different key verifies
	fx := newVerifyFixture(t, pub)
	manifest := manifestBody(linuxX64Asset, strings.Repeat("ab", 32))
	fx.primary.serve(manifest, signManifest(signer, manifest))
	fx.req.DownloadBase = fx.primary.srv.URL

	_, err := VerifyArchiveWithSignedManifests(context.Background(), fx.req, nil)
	var verification *BinaryVerificationError
	if !asVerifyErr(err, &verification) {
		t.Fatalf("wrong signing key must fail as BinaryVerificationError, got %v", err)
	}
}

func TestVerifyManifestsMalformedBase64SigFails(t *testing.T) {
	pub, _ := manifestTestKey(t)
	fx := newVerifyFixture(t, pub)
	manifest := manifestBody(linuxX64Asset, strings.Repeat("ab", 32))
	fx.primary.serve(manifest, "!!!not-base64!!!")
	fx.req.DownloadBase = fx.primary.srv.URL

	_, err := VerifyArchiveWithSignedManifests(context.Background(), fx.req, nil)
	var verification *BinaryVerificationError
	if !asVerifyErr(err, &verification) {
		t.Fatalf("malformed base64 sig must fail as BinaryVerificationError, got %v", err)
	}
}

func TestVerifyManifestsMissingArchiveLineFails(t *testing.T) {
	pub, priv := manifestTestKey(t)
	fx := newVerifyFixture(t, pub)
	manifest := manifestBody("some-other-archive.tar.gz", strings.Repeat("ab", 32))
	fx.primary.serve(manifest, signManifest(priv, manifest))
	fx.req.DownloadBase = fx.primary.srv.URL

	_, err := VerifyArchiveWithSignedManifests(context.Background(), fx.req, nil)
	var verification *BinaryVerificationError
	if !asVerifyErr(err, &verification) {
		t.Fatalf("manifest not covering the archive must fail, got %v", err)
	}
}

func TestVerifyManifestsSigFetchFailureFallsToNextOrigin(t *testing.T) {
	pub, priv := manifestTestKey(t)
	fx := newVerifyFixture(t, pub)
	// Primary serves SUMS but no .sig: the origin counts as
	// unavailable and the fallback must be tried.
	manifest := manifestBody(linuxX64Asset, strings.Repeat("ab", 32))
	fx.primary.sums.Store(manifest) // no sig stored → .sig 404s
	fx.fallback.serve(manifest, signManifest(priv, manifest))
	fx.req.DownloadBase = fx.primary.srv.URL

	verified, err := VerifyArchiveWithSignedManifests(context.Background(), fx.req, nil)
	if err != nil {
		t.Fatalf("fallback must cover a sig-less primary: %v", err)
	}
	if !verified {
		t.Fatal("verified = false, want true")
	}
}

func TestVerifyManifestsUnavailableEverywhereIsNotVerificationFailure(t *testing.T) {
	pub, _ := manifestTestKey(t)
	fx := newVerifyFixture(t, pub) // neither origin serves anything
	fx.req.DownloadBase = fx.primary.srv.URL

	verified, err := VerifyArchiveWithSignedManifests(context.Background(), fx.req, nil)
	if err != nil {
		t.Fatalf("fetch-level unavailability must not be a verification failure: %v", err)
	}
	if verified {
		t.Fatal("verified = true with no manifests anywhere")
	}
}

func TestVerifyManifestsFirstFetchedOriginIsTerminal(t *testing.T) {
	// A primary origin that yields both files but fails verification
	// is terminal: the (good) fallback is NOT consulted — mirroring
	// the upstream try-in-order fetch semantics.
	pub, goodSigner := manifestTestKey(t)
	_, badSigner := manifestTestKey(t)
	fx := newVerifyFixture(t, pub)
	manifest := manifestBody(linuxX64Asset, strings.Repeat("ab", 32))
	fx.primary.serve(manifest, signManifest(badSigner, manifest))
	fx.fallback.serve(manifest, signManifest(goodSigner, manifest))
	fx.req.DownloadBase = fx.primary.srv.URL

	_, err := VerifyArchiveWithSignedManifests(context.Background(), fx.req, nil)
	if err == nil {
		t.Fatal("bad primary signature must fail")
	}
	if fx.fallback.hits.Load() != 0 {
		t.Errorf("verification failure at the first fetched origin must be terminal (fallback hits=%d)", fx.fallback.hits.Load())
	}
}

func TestPinnedManifestKeyDecodes(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(PinnedManifestPubKeyB64)
	if err != nil {
		t.Fatalf("pinned key must be valid base64: %v", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		t.Fatalf("pinned key len = %d, want %d", len(raw), ed25519.PublicKeySize)
	}
}

func TestManifestOriginsOrder(t *testing.T) {
	origins := manifestOrigins("https://mirror.example/", "146.0.7680.177.5")
	if len(origins) != 2 {
		t.Fatalf("origins = %d, want 2", len(origins))
	}
	wantPrimary := "https://mirror.example/chromium-v146.0.7680.177.5/SHA256SUMS"
	if origins[0].sumsURL != wantPrimary {
		t.Errorf("primary sums URL = %q, want %q", origins[0].sumsURL, wantPrimary)
	}
	if origins[0].sigURL != wantPrimary+".sig" {
		t.Errorf("primary sig URL = %q", origins[0].sigURL)
	}
	wantGH := "https://github.com/CloakHQ/cloakbrowser/releases/download/chromium-v146.0.7680.177.5/SHA256SUMS"
	if origins[1].sumsURL != wantGH {
		t.Errorf("fallback sums URL = %q, want %q", origins[1].sumsURL, wantGH)
	}
}

// asVerifyErr asserts err is (or wraps) a *BinaryVerificationError.
func asVerifyErr(err error, target **BinaryVerificationError) bool {
	if err == nil {
		return false
	}
	for err != nil {
		if v, ok := err.(*BinaryVerificationError); ok { //nolint:errorlint // concrete wrap chain
			*target = v
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
