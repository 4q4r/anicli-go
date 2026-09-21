package providers

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
	"golang.org/x/text/encoding/charmap"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AniStarBase is the site root: anistar.org, the live serving mirror of
// the RU anime catalog (the family rotates mirrors — anistarstar.org and
// starlight.serializeran.com answered with broken TLS on 2026-09-20,
// anistar.ru timed out; the mirror list below the search form and the
// catalog's own canonical tags pinned anistar.org on that day). The
// site is a DataLife Engine catalog with a Windows-1251 wire encoding —
// the only provider in the roster whose pages and search form are NOT
// UTF-8 — and its own self-hosted player stack under /test/player2/.
//
// No frozen Python original exists (anidub/anizone precedent): the
// provider was written from the live site, probed 2026-09-20.
const AniStarBase = "https://anistar.org"

// SmokeQuery reports the provider-specific live probe: «боруто» lands
// on the current p2p-player generation; see anistarSmokeQuery.
func (p *AniStar) SmokeQuery() string { return anistarSmokeQuery }

// anistarDubFallback is the dub key for the playlst entries whose title
// carries no suffix — the release's own AniStar voice-over (the site
// leaves it unnamed; «Серия 1» vs «Серия 1 Многоголосая озвучка»).
const anistarDubFallback = "AniStar"

// anistarPlayerPath is the p2p player the release pages iframe in; the
// legacy /test/player2/videoas.php serves the same playlst through a
// playlist_hls.php proxy, the p2p variant exposes the direct per-
// quality links, so it is the one resolved.
const anistarPlayerPath = "/test/player2/videoas_p2p_new.php"

// anistarPlayerRe digs the player iframe's id+hash query out of the
// release page. The hash is per-site session material (one value across
// releases on 2026-09-20) carried in the raw HTML — parsed, never
// pinned. The href may arrive HTML-escaped (&amp;).
var anistarPlayerRe = regexp.MustCompile(`videoas_p2p_new\.php\?id=(\d+)&(?:amp;)?hash=([0-9a-fA-F]{8,64})`)

// anistarPlaylistRe digs the LEGACY player iframe's link parameter out
// of the release page: the playlist_anistar2.php generation (older
// releases — Black Lagoon S2 on 2026-09-20) keys the player by the
// release slug, not by id+hash.
var anistarPlaylistRe = regexp.MustCompile(`playlist_anistar2\.php\?link=([A-Za-z0-9._\-]+\.html)`)

// anistarSpanRe parses the legacy player's playlist spans: each
// `<span onclick="playX('URL' , this)">TITLE</span>` row is one
// (episode, embed) pair; the play function name is deliberately not
// consumed — the embed URL's host routes the resolution.
var anistarSpanRe = regexp.MustCompile(`onclick="[a-zA-Z0-9_]+\('([^']+)'\s*,\s*this\)"[^>]*>([^<]+)<`)

// anistarPlaylstRe slices the player page's `var playlst=[…]` array
// into entries: each entry opens with title+media_id in that order
// (verified across series, movies and one-off team entries on
// 2026-09-20). The array is JavaScript with comments and trailing
// commas — parsed by bounded regex scans, never evaluated.
var anistarPlaylstRe = regexp.MustCompile(`title:"((?:Серия|Фильм)[^"]*)",\s*media_id:"(\d+)"`)

// anistarFilesRe / anistarFilesMp4Re extract one entry's quality
// ladders: files[] is the HLS config (sf2 manifests and the keyed sfv
// edge), files_mp4[] the progressive ladder (tokened sfhd links).
// Either array may be absent; the [^]] body keeps the scan inside the
// array.
var (
	anistarFilesRe    = regexp.MustCompile(`files:\[([^\]]*)\]`)
	anistarFilesMp4Re = regexp.MustCompile(`files_mp4:\[([^\]]*)\]`)
	anistarFileRe     = regexp.MustCompile(`title:"(\d+)",\s*file:"([^"]+)"`)
)

// anistarSeriesRe / anistarMovieRe split a playlst title into the
// episode number and the dub suffix. «Серия 12» and «Серия 12
// Многоголосая озвучка» are the observed series shapes; movies carry a
// single «Фильм» entry (optionally suffixed). The suffix-less entries
// resolve to anistarDubFallback.
var (
	anistarSeriesRe = regexp.MustCompile(`^Серия\s+(\d+)\s*(.*)$`)
	anistarMovieRe  = regexp.MustCompile(`^Фильм\s*(.*)$`)
)

// AniStar is the anistar.org provider (PR77): the RU DLE anime catalog
// with its self-hosted an-media.org player stack, written from the live
// site onto the Go provider contract. Many releases carry several
// voice-over teams; episode listings arrive with every dub's player
// reference in ONE player-page call, so dubs hydrate eagerly (no
// DubsHydrator capability).
//
// The provider deliberately does NOT declare NamePreference — the
// catalog's RU index is the default query routing (yummy precedent).
type AniStar struct {
	Base
}

// anistarSmokeQuery is the declared live probe (PR51 mechanism): the
// shared RU probe «черная лагуна» DOES surface the catalog, but only
// the legacy back-catalog generation — releases whose
// playlist_anistar2 player rides vk.com/myvi.ru embeds, hosts the
// shared extractor factory does not cover (no extractor exists
// upstream either). The declared probe «боруто» surfaces the current
// p2p-player generation (direct an-media HLS/MP4) and exercises the
// supported consumption chain; the legacy generation still resolves
// its episodes honestly and fails loud at stream resolution
// (live-verified 2026-09-20).
const anistarSmokeQuery = "боруто"

// newAniStar builds the provider against the given base. The shared
// netclient is used for every leg (catalog, release page, player page):
// anistar.org answers browser-fingerprint requests normally (the
// occasional Cloudflare challenge keys on bare/robotic User-Agents —
// the netclient profile clears it, verified live 2026-09-20).
func newAniStar(base string, http *netclient.Client) *AniStar {
	return &AniStar{
		Base: Base{
			id:          "anistar",
			name:        "AniStar",
			baseURL:     base,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ru",
			http:        http,
		},
	}
}

// anistar1251Encode renders the query in the site's cp1251 form
// encoding: the DLE full-search matches cp1251 bytes only — the same
// query percent-encoded as UTF-8 answers zero hits (live-verified
// 2026-09-20).
func anistar1251Encode(s string) ([]byte, error) {
	return charmap.Windows1251.NewEncoder().Bytes([]byte(s))
}

// anistar1251Decode renders a cp1251 body as UTF-8. Unmappable bytes
// degrade to U+FFFD, never an error — a broken page must surface as a
// parse result, not a charset panic.
func anistar1251Decode(b []byte) string {
	out, err := charmap.Windows1251.NewDecoder().Bytes(b)
	if err != nil {
		return string(b)
	}
	return string(out)
}

// anistarAbsURL absolutizes the URLs the markup mixes freely (absolute
// https, scheme-relative, root-relative).
func anistarAbsURL(base, u string) string {
	switch {
	case u == "":
		return ""
	case strings.HasPrefix(u, "//"):
		return "https:" + u
	case strings.HasPrefix(u, "/"):
		return base + u
	default:
		return u
	}
}

// Search POSTs the DLE full-search form to the site root
// (do=search&subaction=search&story=<query as cp1251>) and parses the
// answer's .news cards. SearchResult.URL carries the release page URL —
// GetEpisodes keys on it.
//
// Divergence from the site's own search surface (deliberate): the
// answer mixes site-news posts and manga-reader pages into the cards,
// sharing the same /NNNN-slug.html URL shape as releases. The parser
// keeps only cards categorized under the /anime/ section (the news and
// manga cards carry /news/ and /m/ category links) — the search
// fan-out must surface playable releases, and a news pick would fail
// GetEpisodes with a typed miss. DLE caps the form answer at one page
// (~10 cards); no pagination is attempted (yummy/anilib single-page
// precedent).
func (p *AniStar) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	story, err := anistar1251Encode(query)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("%w: query %q is not representable in cp1251: %w", contracts.ErrInvalidInput, query, err))
	}
	form := "do=search&subaction=search&story=" + pyQuote(string(story))
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "POST",
		URL:     p.baseURL + "/",
		Op:      contracts.OpSearch,
		Body:    strings.NewReader(form),
		Headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	})
	if err != nil {
		return nil, err
	}

	doc, parseErr := goquery.NewDocumentFromReader(strings.NewReader(anistar1251Decode(resp.Body)))
	if parseErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("parse search page: %w", parseErr))
	}

	results := make([]contracts.SearchResult, 0, 10)
	seen := map[string]bool{}
	doc.Find("div.news").Each(func(_ int, card *goquery.Selection) {
		// The /anime/ category link is the release marker; news posts
		// and manga pages carry /news/ and /m/ sections instead.
		if card.Find(".tags a[href*='/anime/']").Length() == 0 {
			return
		}
		link := card.Find(".title_left a[href]").First()
		href, ok := link.Attr("href")
		if !ok || href == "" {
			return
		}
		url := anistarAbsURL(p.baseURL, href)
		if seen[url] {
			return
		}
		seen[url] = true
		title := strings.TrimSpace(link.Text())
		poster, _ := card.Find("img.main-img").Attr("src")
		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      url,
			SourceID: p.ID(),
			Poster:   anistarAbsURL(p.baseURL, poster),
		})
	})
	return results, nil
}

// anistarFile is one quality rung of a playlst entry's ladder.
type anistarFile struct {
	quality string
	url     string
}

// anistarEntry is one playlst array row: a (episode, dub) pair with
// its per-quality links.
type anistarEntry struct {
	title    string
	mediaID  string
	files    []anistarFile // the HLS config (files:[…])
	filesMp4 []anistarFile // the progressive config (files_mp4:[…])
}

// anistarParsePlaylst scans the player page's var playlst JS array
// into entries. Entry bodies are bounded by the next entry's title —
// the array's formatting (tabs, //0 comments, trailing commas) never
// crosses a title boundary in the observed pages.
func anistarParsePlaylst(js string) []anistarEntry {
	locs := anistarPlaylstRe.FindAllStringSubmatchIndex(js, -1)
	entries := make([]anistarEntry, 0, len(locs))
	for i, loc := range locs {
		end := len(js)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		block := js[loc[0]:end]
		entry := anistarEntry{
			title:   js[loc[2]:loc[3]],
			mediaID: js[loc[4]:loc[5]],
		}
		entry.files = anistarParseFiles(anistarFilesRe.FindStringSubmatch(block))
		entry.filesMp4 = anistarParseFiles(anistarFilesMp4Re.FindStringSubmatch(block))
		entries = append(entries, entry)
	}
	return entries
}

// anistarParseFiles extracts the (quality, url) pairs of one ladder
// array body (nil submatch → absent array).
func anistarParseFiles(sub []string) []anistarFile {
	if sub == nil {
		return nil
	}
	pairs := anistarFileRe.FindAllStringSubmatch(sub[1], -1)
	out := make([]anistarFile, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, anistarFile{quality: pair[1], url: pair[2]})
	}
	return out
}

// anistarSplitTitle splits a playlst title into the episode number and
// the dub suffix («» empty for the release's own voice-over). Movies
// count as episode 1 (the movie numbering convention).
func anistarSplitTitle(title string) (num, suffix string) {
	if m := anistarSeriesRe.FindStringSubmatch(title); m != nil {
		return m[1], strings.TrimSpace(m[2])
	}
	if m := anistarMovieRe.FindStringSubmatch(title); m != nil {
		return "1", strings.TrimSpace(m[1])
	}
	return title, ""
}

// anistarPlayerProbe fetches the release page once and reports which
// player generation it iframes: the p2p player URL (id+hash) or the
// legacy playlist URL (link={slug}). A page carrying neither (site
// news share the .html URL shape) is the typed not-found.
func (p *AniStar) anistarPlayerProbe(ctx context.Context, animeURL string) (string, bool, error) {
	resp, err := p.http.Get(ctx, animeURL, nil)
	if err != nil {
		return "", false, err
	}
	page := anistar1251Decode(resp.Body)
	if m := anistarPlayerRe.FindStringSubmatch(page); m != nil {
		return p.baseURL + anistarPlayerPath + "?id=" + m[1] + "&hash=" + m[2], false, nil
	}
	if m := anistarPlaylistRe.FindStringSubmatch(page); m != nil {
		return p.baseURL + "/playlist_anistar2.php?link=" + m[1], true, nil
	}
	return "", false, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
		fmt.Errorf("%w: no anistar player on page %s", contracts.ErrNotFound, animeURL))
}

// GetEpisodes resolves the release page's player and lists episodes.
// Two player generations exist and both are handled:
//
//   - the p2p player (videoas_p2p_new.php): entries group by the
//     «Серия N» number (first-seen order — the array interleaves the
//     dub teams episode by episode), each entry's title suffix keys
//     the dub in RawEmbeds, the suffix-less entries key as
//     anistarDubFallback. RawEmbeds values are the player URL with the
//     entry's media_id as the #fragment — the stable per-(episode,
//     dub) identity ResolveStream re-resolves (the player page
//     re-fetch at resolve time also refreshes the tokened sfhd/sfv
//     links, whose embedded expiry moves).
//   - the legacy playlist player (playlist_anistar2.php): the
//     #PlayList spans map one episode to one embed URL (myvi/vk
//     re-hosts and friends); the embed URL itself is the RawEmbeds
//     value, resolved through the shared extractor factory.
func (p *AniStar) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	type titleRef struct {
		title string
		ref   string
	}
	playerURL, legacy, perr := p.anistarPlayerProbe(ctx, animeURL)
	if perr != nil {
		return nil, perr
	}
	var pairs []titleRef
	if legacy {
		playlist, err := p.http.Get(ctx, playerURL, map[string]string{"Referer": animeURL})
		if err != nil {
			return nil, err
		}
		spans := anistarSpanRe.FindAllStringSubmatch(anistar1251Decode(playlist.Body), -1)
		if len(spans) == 0 {
			return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, playlist.StatusCode,
				fmt.Errorf("%w: empty playlist for %s", contracts.ErrNotFound, animeURL))
		}
		for _, span := range spans {
			// Legacy pages entity-encode the URL query (&amp;); the
			// ref must be unescaped before it rides into RawEmbeds,
			// or the extractor factory resolves a mangled query
			// (PR82 review nit #9 — behavior change is the fix).
			pairs = append(pairs, titleRef{title: strings.TrimSpace(span[2]), ref: html.UnescapeString(span[1])})
		}
	} else {
		player, err := p.http.Get(ctx, playerURL, map[string]string{"Referer": animeURL})
		if err != nil {
			return nil, err
		}
		entries := anistarParsePlaylst(string(player.Body))
		if len(entries) == 0 {
			return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, player.StatusCode,
				fmt.Errorf("%w: empty playlst for %s", contracts.ErrNotFound, animeURL))
		}
		for _, entry := range entries {
			pairs = append(pairs, titleRef{title: entry.title, ref: playerURL + "#" + entry.mediaID})
		}
	}

	type group struct {
		num  string
		dubs map[string][]string
	}
	order := make([]string, 0, 16)
	byNum := make(map[string]*group, 16)
	for _, pair := range pairs {
		num, suffix := anistarSplitTitle(pair.title)
		dub := suffix
		if dub == "" {
			dub = anistarDubFallback
		}
		g, ok := byNum[num]
		if !ok {
			g = &group{num: num, dubs: map[string][]string{}}
			byNum[num] = g
			order = append(order, num)
		}
		g.dubs[dub] = append(g.dubs[dub], pair.ref)
	}

	episodes := make([]contracts.Episode, 0, len(order))
	for _, num := range order {
		g := byNum[num]
		episodes = append(episodes, contracts.Episode{
			Num:       g.num,
			RawID:     g.num,
			RawEmbeds: g.dubs,
		})
	}
	return episodes, nil
}

// anistarStreamHeaders is the load-bearing playback header set: the
// an-media edge answers 403 on manifest requests without the site
// Referer (verified live 2026-09-20).
func (p *AniStar) anistarStreamHeaders() map[string]string {
	return map[string]string{"Referer": p.baseURL + "/"}
}

// anistarStreamType labels a link by its URL shape: the sf2 ladders
// end in index.m3u8 manifests, everything else (sfhd progressive, the
// keyed sfv edge) plays as mp4.
func anistarStreamType(u string) string {
	if strings.Contains(u, ".m3u8") {
		return "m3u8"
	}
	return "mp4"
}

// ResolveStream re-fetches the player page and resolves the dub's
// media_id entry into per-quality sources. Precedence within a
// quality: the files[] HLS ladder wins the key, files_mp4[] fills the
// qualities the HLS ladder lacks (the movie capture: files[] carries
// both rungs, files_mp4[] adds nothing).
func (p *AniStar) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	refs, ok := episode.RawEmbeds[dubID]
	if !ok || len(refs) == 0 {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}},
			contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
				fmt.Errorf("%w: dub %q has no player references on episode %s", contracts.ErrNotFound, dubID, episode.Num))
	}

	stream := contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}}
	var firstErr error
	for _, ref := range refs {
		anchor := strings.LastIndex(ref, "#")
		if anchor < 0 {
			// The legacy-player shape: the RawEmbeds value IS the embed
			// URL — resolve through the shared extractor factory. The
			// factory settles unknown hosts (vk.com, myvi.ru) with an
			// empty, error-free answer — surfaced here as the loud
			// typed failure naming the host, never a silent zero.
			sources, err := resolveEmbeds(ctx, p.http, []string{ref})
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if len(sources) == 0 {
				if firstErr == nil {
					firstErr = p.anistarUnsupportedEmbed(ref)
				}
				continue
			}
			for quality, src := range sources {
				stream.Links[quality] = src // dict.update: later links overwrite
			}
			continue
		}
		playerURL, mediaID := ref[:anchor], ref[anchor+1:]

		entries, err := p.fetchPlaylst(ctx, playerURL)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		var entry *anistarEntry
		for i := range entries {
			if entries[i].mediaID == mediaID {
				entry = &entries[i]
				break
			}
		}
		if entry == nil {
			if firstErr == nil {
				firstErr = contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
					fmt.Errorf("%w: media_id %s absent from the playlst", contracts.ErrNotFound, mediaID))
			}
			continue
		}

		headers := p.anistarStreamHeaders()
		for _, f := range entry.files {
			stream.Links[f.quality] = contracts.VideoSource{
				URL: f.url, Quality: f.quality, Type: anistarStreamType(f.url), Headers: headers,
			}
		}
		for _, f := range entry.filesMp4 {
			if _, taken := stream.Links[f.quality]; taken {
				continue // the HLS ladder wins the quality key
			}
			stream.Links[f.quality] = contracts.VideoSource{
				URL: f.url, Quality: f.quality, Type: anistarStreamType(f.url), Headers: headers,
			}
		}
	}

	if len(stream.Links) == 0 {
		if firstErr == nil {
			firstErr = contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
				fmt.Errorf("%w: no playable links for dub %q episode %s", contracts.ErrExtractFailed, dubID, episode.Num))
		}
		return stream, firstErr
	}
	return stream, nil
}

// anistarUnsupportedEmbed builds the typed failure for embed hosts the
// shared extractor factory does not cover (the legacy playlists ride
// myvi.ru and vk.com re-hosts): the host is named so the failure is
// actionable, never a silent zero-link answer.
func (p *AniStar) anistarUnsupportedEmbed(ref string) error {
	host := ref
	if parsed, err := url.Parse(ref); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	return contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
		fmt.Errorf("%w: no extractor for embed host %s", contracts.ErrExtractFailed, host))
}

// fetchPlaylst fetches one player page and parses its playlst array.
// The site Referer rides along (the iframe context the browser sends).
func (p *AniStar) fetchPlaylst(ctx context.Context, playerURL string) ([]anistarEntry, error) {
	player, err := p.http.Get(ctx, playerURL, map[string]string{"Referer": p.baseURL + "/"})
	if err != nil {
		return nil, fmt.Errorf("extractor:anistar: %w", err)
	}
	entries := anistarParsePlaylst(string(player.Body))
	if len(entries) == 0 {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, player.StatusCode,
			fmt.Errorf("%w: empty playlst at %s", contracts.ErrExtractFailed, playerURL))
	}
	return entries, nil
}
