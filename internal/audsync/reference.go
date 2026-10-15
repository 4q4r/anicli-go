package audsync

// The v2 reference hierarchy (the owner's ruling, 2026-10-15: «Делай
// все также, только без донора»): the reference is picked from the
// video's OWN streams, never from a donor track —
//
//  1. subtitles (the ideal): ASS dialogue intervals → sub_norm;
//  2. a japanese-tagged embedded audio track → vocal envelope;
//  3. any other embedded audio track (preferably one whose language
//     differs from the donors') — the first non-silent candidate;
//  4. nothing usable → typed ErrNoReference: the download proceeds
//     unsynchronized (the analysis NEVER fails a download).
//
// The reference is computed ONCE per episode and reused for every
// donor.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// jpnLangs are the language tags that mark the original Japanese
// track (case-insensitive).
var jpnLangs = []string{"jpn", "ja", "japanese"}

// streamInfo is one ffprobe stream the hierarchy cares about, with
// its TYPE-RELATIVE index (the -map 0:s:N / 0:a:N numbering).
type streamInfo struct {
	relIndex int
	lang     string
}

// probeStreams runs ffprobe -show_streams through the seam (on the
// analysis Budget — the probe is a fast metadata read) and splits the
// container's subtitle and audio tracks, in container order.
func (a *Analyzer) probeStreams(ctx context.Context, src contracts.VideoSource) ([]streamInfo, []streamInfo, error) {
	args := []string{"-v", "error", "-show_streams", "-of", "json"}
	args = append(args, headerBlob(src.Headers)...)
	args = append(args, src.URL)
	out, _, err := a.exec(ctx, a.ffprobe, args)
	if err != nil {
		return nil, nil, err
	}
	var parsed struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Tags      struct {
				Language string `json:"language"`
			} `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, nil, fmt.Errorf("audsync: stream probe: %w", err)
	}
	var subs, auds []streamInfo
	for _, s := range parsed.Streams {
		switch s.CodecType {
		case "subtitle":
			subs = append(subs, streamInfo{relIndex: len(subs), lang: strings.ToLower(s.Tags.Language)})
		case "audio":
			auds = append(auds, streamInfo{relIndex: len(auds), lang: strings.ToLower(s.Tags.Language)})
		}
	}
	return subs, auds, nil
}

// assExtractArgs renders the ASS extraction (the script's command,
// verbatim): the whole subtitle track converted to ASS on the pipe.
// The trailing `?` keeps an unreadable track from failing — an empty
// stdout is the honest "no intervals" answer.
func assExtractArgs(src contracts.VideoSource, track int) []string {
	args := []string{"-v", "error"}
	args = append(args, headerBlob(src.Headers)...)
	args = append(args, "-i", src.URL, "-map",
		fmt.Sprintf("0:s:%d?", track), "-f", "ass", "pipe:")
	return args
}

// envDecodeArgs renders the ENVELOPE decode (the script's
// get_vocal_envelope command): the WHOLE source's audio to the pipe
// at 8 kHz STEREO s16le. There is deliberately NO -ss/-t: a whole
// decode follows the true decode-order timeline (the HLS rationale
// carried over from v1), and the mid/side separation needs both
// channels. track < 0 decodes the default (best) audio — the donor
// leg; track ≥ 0 pins a specific embedded track — the reference leg.
func envDecodeArgs(src contracts.VideoSource, track int) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	args = append(args, headerBlob(src.Headers)...)
	args = append(args, "-i", src.URL)
	if track >= 0 {
		args = append(args, "-map", fmt.Sprintf("0:a:%d", track))
	}
	args = append(args, "-vn", "-ac", "2", "-ar", "8000", "-f", "s16le", "pipe:")
	return args
}

// decodeEnvelope runs one envelope decode through the seam and builds
// the 72000-bin vocal envelope.
func (a *Analyzer) decodeEnvelope(ctx context.Context, src contracts.VideoSource, track int) ([]float64, error) {
	data, _, err := a.exec(ctx, a.ffmpeg, envDecodeArgs(src, track))
	if err != nil {
		return nil, err
	}
	return envelopeFromPCM(data), nil
}

// Reference builds the episode's reference envelope from the video's
// own streams. Budgets, both derived from the parent context: the
// stream probe rides the per-analysis Budget (a fast metadata read);
// the ASS extractions and every candidate decode share ONE
// DecodeBudget — a remote ASS leg converts the whole subtitle track
// off the full media stream (whole-source-decode work), and N legs
// must not multiply the wait. Every failure wraps ErrNoReference.
func (a *Analyzer) Reference(ctx context.Context, video contracts.VideoSource, donorLangs ...string) (Reference, error) {
	probeCtx, cancelProbe := context.WithTimeout(ctx, a.budget)
	defer cancelProbe()
	subs, auds, err := a.probeStreams(probeCtx, video)
	if err != nil {
		return Reference{}, fmt.Errorf("%w (stream probe: %w)", ErrNoReference, err)
	}

	// 1. Subtitles — the script's candidate order (1, 0), the best
	// (most-intervals) track wins, and a track below the usable
	// floor is not a reference at all.
	decodeCtx, cancelDecode := context.WithTimeout(ctx, a.decodeBudget)
	defer cancelDecode()
	var best []assInterval
	for _, track := range []int{1, 0} {
		if track >= len(subs) {
			continue // the `?` answer, precomputed: an absent track yields nothing
		}
		out, _, err := a.exec(decodeCtx, a.ffmpeg, assExtractArgs(video, track))
		if err != nil || len(out) == 0 {
			continue
		}
		if ivs := parseASSIntervals(string(out)); len(ivs) > len(best) {
			best = ivs
		}
	}
	if len(best) >= MinSubIntervals {
		return Reference{
			Kind:     RefSubs,
			Envelope: buildSubNorm(best),
			MinLag:   SubLagMin,
			MaxLag:   SubLagMax,
		}, nil
	}

	// 2/3. Audio candidates: japanese-tagged tracks first (the
	// original language), then everything else preferring a language
	// no donor speaks (the dub is usually rus — an eng commentary
	// outranks another rus track), then the rest. The first
	// non-silent candidate wins.
	candidates := make([]streamInfo, 0, len(auds))
	var rest []streamInfo
	for _, tr := range auds {
		if isJpn(tr.lang) {
			candidates = append(candidates, tr)
		} else {
			rest = append(rest, tr)
		}
	}
	donorSet := make(map[string]bool, len(donorLangs))
	for _, l := range donorLangs {
		donorSet[strings.ToLower(l)] = true
	}
	var differing []streamInfo
	for _, tr := range rest {
		if tr.lang != "" && !donorSet[tr.lang] {
			differing = append(differing, tr)
		}
	}
	candidates = append(candidates, differing...)
	for _, tr := range rest {
		if tr.lang == "" || donorSet[tr.lang] {
			candidates = append(candidates, tr)
		}
	}
	for _, tr := range candidates {
		env, err := a.decodeEnvelope(decodeCtx, video, tr.relIndex)
		if err != nil {
			if ctx.Err() != nil || decodeCtx.Err() != nil {
				return Reference{}, fmt.Errorf("%w (reference decode: %w)", ErrNoReference, decodeCtx.Err())
			}
			continue // an undecodable candidate is not evidence; try the next
		}
		if envelopeActivity(env) < MinEnvBins {
			continue // silent: the script's «первый не-тихий» rule
		}
		return Reference{
			Kind:     RefAudio,
			Envelope: env,
			MinLag:   AudioLagMin,
			MaxLag:   AudioLagMax,
		}, nil
	}
	return Reference{}, ErrNoReference
}

// isJpn reports whether a language tag marks the Japanese track.
func isJpn(lang string) bool {
	for _, l := range jpnLangs {
		if lang == l {
			return true
		}
	}
	return false
}
