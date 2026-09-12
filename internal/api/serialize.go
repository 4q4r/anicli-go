package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/shikimori"
	"github.com/an0nx/anicli-go/internal/storage"
)

// Serialization of API payloads, ported from api_server.py's
// _build_artwork/_serialize_* helpers. JSON keys match the Python
// contract exactly.

// placeholderPoster is the fallback artwork URL (python
// _PLACEHOLDER_POSTER).
const placeholderPoster = "https://placehold.co/600x900?text=No+Poster"

// imageCacheTTLSeconds rides inside artwork cache hints.
const imageCacheTTLSeconds = 86400

// cacheHash is the deterministic token part for cache keys (python
// _cache_hash: full sha256 hex).
func cacheHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// buildArtwork renders the normalized artwork object plus cache hints
// (python _build_artwork + _image_cache_hints).
func buildArtwork(poster string, versionSeed string) map[string]any {
	normalized := poster
	if normalized == "" {
		normalized = placeholderPoster
	}
	seedSum := sha256.Sum256([]byte(versionSeed))
	artworkVersion := hex.EncodeToString(seedSum[:])[:16]
	return map[string]any{
		"poster":              normalized,
		"cover":               nil,
		"backdrop":            nil,
		"thumb":               nil,
		"dominant_color":      nil,
		"blurhash":            nil,
		"image_cache_ttl_sec": imageCacheTTLSeconds,
		"etag_seed":           artworkVersion,
		"artwork_version":     artworkVersion,
	}
}

// serializeHistoryRecord renders one anime_progress row with its
// sources (python _serialize_history_record).
func serializeHistoryRecord(rec *storage.AnimeProgress, sources []storage.AnimeSource) map[string]any {
	updated := ""
	if !rec.UpdatedAt.IsZero() {
		updated = rec.UpdatedAt.UTC().Format(time.RFC3339)
	}
	items := make([]map[string]any, 0, len(sources))
	for _, src := range sources {
		items = append(items, map[string]any{
			"id":         src.ID,
			"source_id":  src.SourceID,
			"source_url": src.SourceURL,
		})
	}
	return map[string]any{
		"id":                rec.ID,
		"title":             rec.Title,
		"artwork":           buildArtwork(deref(rec.Poster), fmt.Sprintf("history:%d:%s", rec.ID, updated)),
		"current_episode":   rec.CurrentEpisode,
		"video_dub":         rec.VideoDub,
		"audio_dub":         rec.AudioDub,
		"shikimori_title":   rec.ShikimoriTitle,
		"bound_title":       rec.BoundTitle,
		"shikimori_id":      rec.ShikimoriID,
		"shikimori_rate_id": rec.ShikimoriRateID,
		"shikimori_status":  rec.ShikimoriStatus,
		"score":             rec.Score,
		"total_episodes":    rec.TotalEpisodes,
		"rewatches":         rec.Rewatches,
		"needs_correction":  rec.NeedsCorrection,
		"dirty":             rec.Dirty,
		"updated_at":        nilIfEmpty(updated),
		"sources":           items,
	}
}

// trackKeysForEpisode normalizes per-episode track keys by provider
// source type (python _track_keys_for_episode): video-only sources put
// every key under video_keys, audio-only under audio_keys, both under
// both; mixed_possible flags episodes where a video+audio pairing could
// differ from a same-key single track.
func trackKeysForEpisode(episode contracts.Episode, sourceType contracts.SourceType) (video, audio []string, mixed bool) {
	rawKeys := make([]string, 0, len(episode.RawEmbeds))
	for key := range episode.RawEmbeds {
		rawKeys = append(rawKeys, key)
	}
	sort.Strings(rawKeys)
	if len(rawKeys) == 0 {
		return nil, nil, false
	}

	switch sourceType {
	case contracts.SourceTypeVideo:
		video, audio = rawKeys, []string{}
	case contracts.SourceTypeAudio:
		video, audio = []string{}, rawKeys
	default:
		video, audio = rawKeys, rawKeys
	}

	mixed = len(video) > 0 && len(audio) > 0 && !equalSets(video, audio) || len(video) > 1
	return video, audio, mixed
}

// serializeEpisode renders the episodes DTO (python _serialize_episode)
// — track keys only, never raw embed URLs.
func serializeEpisode(ep contracts.Episode, sourceType contracts.SourceType) map[string]any {
	video, audio, mixed := trackKeysForEpisode(ep, sourceType)
	return map[string]any{
		"num":            ep.Num,
		"title":          ep.Title,
		"raw_id":         ep.RawID,
		"video_keys":     video,
		"audio_keys":     audio,
		"mixed_possible": mixed,
	}
}

// equalSets reports whether two sorted slices hold the same values.
func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// providerIDFromTrackKey extracts the source id from "[source] dub"
// keys (python _provider_id_from_track_key).
func providerIDFromTrackKey(trackKey, defaultSource string) string {
	if strings.HasPrefix(trackKey, "[") {
		if idx := strings.Index(trackKey, "]"); idx > 0 {
			if inner := strings.TrimSpace(trackKey[1:idx]); inner != "" {
				return inner
			}
		}
	}
	return defaultSource
}

// streamType normalizes the stream kind hint (python _stream_type).
func streamType(url, hinted string) string {
	switch strings.ToLower(strings.TrimSpace(hinted)) {
	case "m3u8", "hls":
		return "hls"
	case "mp4":
		return "mp4"
	case "mpd", "dash":
		return "dash"
	}
	lower := strings.ToLower(url)
	switch {
	case strings.Contains(lower, ".m3u8"):
		return "hls"
	case strings.Contains(lower, ".mpd"):
		return "dash"
	case strings.Contains(lower, ".mp4"):
		return "mp4"
	}
	return "unknown"
}

// animeRowMap renders a shikimori.Anime into the python anime-row dict
// shape consumed by the home card builders (keys: name, russian, image,
// score, description, genres, kind, status, year, episodes,
// episodes_aired, next_episode, next_episode_at, url, aired_on).
func animeRowMap(a *shikimori.Anime) map[string]any {
	image := map[string]any{}
	if a.Image.Original != "" {
		image["original"] = a.Image.Original
	}
	if a.Image.Main != "" {
		image["main"] = a.Image.Main
	}
	if a.Image.Preview != "" {
		image["preview"] = a.Image.Preview
	}
	if a.Image.X96 != "" {
		image["x96"] = a.Image.X96
	}
	if a.Image.X48 != "" {
		image["x48"] = a.Image.X48
	}

	genres := make([]any, 0, len(a.Genres))
	for _, g := range a.Genres {
		genres = append(genres, map[string]any{"name": g.Name, "russian": g.Russian})
	}

	year := any(nil)
	if y, err := strconv.Atoi(firstDigits(a.AiredOn)); err == nil && a.AiredOn != "" {
		year = y
	}

	return map[string]any{
		"id":              a.ID,
		"name":            a.Name,
		"russian":         a.Russian,
		"image":           image,
		"score":           a.Score,
		"description":     a.Description,
		"genres":          genres,
		"kind":            a.Kind,
		"status":          a.Status,
		"year":            year,
		"episodes":        a.Episodes,
		"episodes_aired":  a.EpisodesAired,
		"next_episode":    a.NextEpisode,
		"next_episode_at": a.NextEpisodeAt,
		"url":             a.URL,
		"aired_on":        a.AiredOn,
	}
}

// firstDigits extracts the leading digit run of s ("" when none).
func firstDigits(s string) string {
	start := -1
	end := -1
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			if start == -1 {
				start = i
			}
			end = i + 1
		} else if start != -1 {
			break
		}
	}
	if start == -1 {
		return ""
	}
	return s[start:end]
}

// paginate applies the offset-cursor pagination (python _paginate).
func paginate[T any](items []T, limit int, cursor string) ([]T, *string) {
	offset := 0
	if cursor != "" {
		if v, err := strconv.Atoi(cursor); err == nil && v > 0 {
			offset = v
		}
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	page := items[offset:end]
	nextOffset := offset + limit
	if nextOffset < len(items) {
		next := strconv.Itoa(nextOffset)
		return page, &next
	}
	return page, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
