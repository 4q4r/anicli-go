package cfbrowser

import "testing"

func TestIsChallengePage(t *testing.T) {
	cases := []struct {
		name      string
		title     string
		body      string
		challenge bool
	}{
		{"cf just a moment", "Just a moment...", `<html><script src="/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1">`, true},
		{"cf attention required", "Attention Required! | Cloudflare", `<body>cf-chl-widget-1234</body>`, true},
		{"checking browser", "Checking your browser before accessing", `<script>challenge-platform</script>`, true},
		{"turnstile widget only", "Some site", `<div class="cf-turnstile" data-sitekey="x">`, true},
		{"normal page", "AnimeGo — аниме онлайн", `<html><body>episodes list</body></html>`, false},
		{"body mentions cf in prose", "Review", `<p>we use cloudflare cdn</p>`, false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsChallengePage(tc.title, tc.body); got != tc.challenge {
				t.Errorf("IsChallengePage(%q, …) = %v, want %v", tc.title, got, tc.challenge)
			}
		})
	}
}

func TestSolvedState(t *testing.T) {
	cases := []struct {
		name                 string
		title, body          string
		hasCFClearanceCookie bool
		solved               bool
	}{
		{"clearance cookie wins even on challengey title", "Just a moment...", "challenge-platform", true, true},
		{"clean page without cookie", "AnimeGo", "<body>ok</body>", false, true},
		{"challenge without cookie", "Just a moment...", "challenge-platform", false, false},
		{"clean page with cookie", "AnimeGo", "<body>ok</body>", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SolvedState(tc.title, tc.body, tc.hasCFClearanceCookie); got != tc.solved {
				t.Errorf("SolvedState = %v, want %v", got, tc.solved)
			}
		})
	}
}
