package extractors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/crypto"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// gogoPlayExtractor ports GogoPlayExtractor (extractors.py:441-520), the
// gogoanime player family. The AES primitives are the ported
// internal/crypto ones (Python anicli.core.crypto).
//
// Divergence from Python (test-enabling, prod-equivalent): the ajax URL
// is built as scheme://netloc of the embed (extractors.py:492 hardcodes
// https://); production embeds are https, so wire behavior matches.
type gogoPlayExtractor struct {
	http *netclient.Client
}

var (
	gogoKeysRe     = regexp.MustCompile(`(?:container|videocontent)-(\d+)`)
	gogoDataValRe  = regexp.MustCompile(`data-value="([^"]+)"`)
	gogoLabelNumRe = regexp.MustCompile(`(\d+)`)
)

// Name identifies the extractor.
func (e *gogoPlayExtractor) Name() string { return "gogoplay" }

// Matches ports the six-way URL rule (extractors.py:448). "turbovid"
// hosts match nothing — the frozen Python factory has no rule for them.
func (e *gogoPlayExtractor) Matches(u string) bool {
	return containsAny("gogoplay", "playtaku", "playgo", "goload", "streaming.php", "embedplus")(u)
}

// Extract resolves a gogoplay-family embed URL via the encrypt-ajax
// handshake.
func (e *gogoPlayExtractor) Extract(ctx context.Context, rawURL string) (map[string]contracts.VideoSource, error) {
	shape := func(reason string) error {
		return fmt.Errorf("extractor:gogoplay: %w: %s", contracts.ErrExtractFailed, reason)
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, shape(fmt.Sprintf("unparseable embed url: %v", err))
	}
	contentID := parsed.Query().Get("id")
	if contentID == "" {
		// Python returned {} (extractors.py:456-457); the port surfaces
		// the typed shape error per the task ruling.
		return nil, shape("embed URL carries no id parameter")
	}

	resp, err := e.http.Get(ctx, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("extractor:gogoplay: %w", err)
	}

	// Three 16-digit keys off the (container|videocontent)- markers
	// (extractors.py:462-469).
	keys := gogoKeysRe.FindAllStringSubmatch(string(resp.Body), -1)
	if len(keys) < 3 {
		return nil, shape("fewer than three AES key markers on the page")
	}
	encryptionKey := []byte(keys[0][1])
	iv := []byte(keys[1][1])
	decryptionKey := []byte(keys[2][1])

	dataValue := gogoDataValRe.FindStringSubmatch(string(resp.Body))
	if dataValue == nil {
		return nil, shape("no data-value attribute on the page")
	}

	decryptedParams, err := crypto.AESDecrypt(dataValue[1], encryptionKey, iv)
	if err != nil {
		return nil, shape(fmt.Sprintf("decrypt data-value: %v", err))
	}
	encryptedID, err := crypto.AESEncrypt(contentID, encryptionKey, iv)
	if err != nil {
		return nil, shape(fmt.Sprintf("encrypt id: %v", err))
	}

	// component = <decrypted params>&id=<encrypted>&alias=<content id>
	// (extractors.py:486-492). Python interpolates the raw base64 into
	// the query — bug-compatible, no re-encoding.
	component := decryptedParams + "&id=" + encryptedID + "&alias=" + contentID
	ajaxURL := parsed.Scheme + "://" + parsed.Host + "/encrypt-ajax.php?" + component

	ajaxResp, err := e.http.Get(ctx, ajaxURL,
		map[string]string{"X-Requested-With": "XMLHttpRequest"})
	if err != nil {
		return nil, fmt.Errorf("extractor:gogoplay: %w", err)
	}
	var ajaxData struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(ajaxResp.Body, &ajaxData); err != nil {
		return nil, shape(fmt.Sprintf("ajax json: %v", err))
	}

	finalStr, err := crypto.AESDecrypt(ajaxData.Data, decryptionKey, iv)
	if err != nil {
		return nil, shape(fmt.Sprintf("decrypt ajax payload: %v", err))
	}
	var final struct {
		Source []struct {
			File  string `json:"file"`
			Label string `json:"label"`
		} `json:"source"`
		SourceBk []struct {
			File string `json:"file"`
		} `json:"source_bk"`
	}
	if err := json.Unmarshal([]byte(finalStr), &final); err != nil {
		return nil, shape(fmt.Sprintf("source json: %v", err))
	}

	results := map[string]contracts.VideoSource{}
	for _, source := range final.Source {
		if source.File == "" {
			continue
		}
		// Label digits, default 720 (extractors.py:505-507).
		q := 720
		if m := gogoLabelNumRe.FindStringSubmatch(source.Label); m != nil {
			parsed, convErr := strconv.Atoi(m[1])
			if convErr == nil {
				q = parsed
			}
		}
		qs := strconv.Itoa(q)
		results[qs] = contracts.VideoSource{URL: source.File, Quality: qs, Type: "m3u8"}
	}
	// Backup only fills a missing 720 (extractors.py:511-515).
	for _, source := range final.SourceBk {
		if source.File != "" {
			if _, ok := results["720"]; !ok {
				results["720"] = contracts.VideoSource{URL: source.File, Quality: "720", Type: "m3u8"}
			}
			break
		}
	}
	if len(results) == 0 {
		return nil, shape("decrypted payload carries no sources")
	}
	return results, nil
}
