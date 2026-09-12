// Package contracts defines the provider-facing DTOs, the consumer-side
// Provider interface and the provider error taxonomy, ported from the frozen
// Python original (anicli-py anicli/core/models.py and anicli/exceptions.py).
//
// JSON field names intentionally mirror the Python pydantic serialization
// (snake_case) so captured fixtures and API payloads stay interchangeable.
package contracts

// SourceType classifies what kind of content a source provides.
type SourceType string

const (
	// SourceTypeVideo marks sources providing video streams only.
	SourceTypeVideo SourceType = "video"
	// SourceTypeAudio marks sources providing audio streams only.
	SourceTypeAudio SourceType = "audio"
	// SourceTypeBoth marks sources providing video and audio streams.
	SourceTypeBoth SourceType = "both"
)

// SearchResult is a single anime search hit.
type SearchResult struct {
	// Title is the name of the anime.
	Title string `json:"title"`
	// URL is the direct URL to the anime page or a unique slug.
	URL string `json:"url"`
	// SourceID identifies the provider, e.g. "animego".
	SourceID string `json:"source_id"`
	// Poster is a URL to the poster image, empty when unknown.
	Poster string `json:"poster,omitempty"`
	// Meta carries arbitrary additional metadata.
	Meta map[string]any `json:"meta,omitempty"`
}

// Episode is a single episode of an anime.
type Episode struct {
	// Num is the episode number; a string because providers use values
	// like "OVA" or "2.5".
	Num string `json:"num"`
	// Title is the episode title, empty when the provider has none.
	Title string `json:"title,omitempty"`
	// RawID is the internal identifier or slug used by the provider.
	RawID string `json:"raw_id"`
	// RawEmbeds maps dub name -> list of raw embed URLs.
	RawEmbeds map[string][]string `json:"raw_embeds,omitempty"`
}

// VideoSource is a concrete playable link with a quality label.
type VideoSource struct {
	// URL is the direct URL to the video file or stream.
	URL string `json:"url"`
	// Quality is the vertical resolution label, e.g. "1080", "720".
	Quality string `json:"quality,omitempty"`
	// Headers are HTTP headers required to play the video (load-bearing
	// for mpv: Referer, Origin, cookies).
	Headers map[string]string `json:"headers,omitempty"`
	// ExtraMPVOpts are additional mpv command-line options for this source.
	ExtraMPVOpts []string `json:"extra_mpv_opts,omitempty"`
	// Type is the content type, e.g. "m3u8", "mp4", "mpd"; empty when
	// unknown.
	Type string `json:"type,omitempty"`
}

// MediaStream is the set of quality links for one dubbing/translation.
type MediaStream struct {
	// DubName is the voice-over studio or language name.
	DubName string `json:"dub_name"`
	// Links maps quality label -> VideoSource.
	Links map[string]VideoSource `json:"links,omitempty"`
}

// DubOption is one selectable dub for an episode.
type DubOption struct {
	// ID is the stable identifier consumed by Provider.ResolveStream.
	ID string `json:"id"`
	// Name is the human-readable dub name.
	Name string `json:"name"`
}
