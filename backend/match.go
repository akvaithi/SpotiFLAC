package backend

import (
	"regexp"
	"strings"
	"unicode"
)

// Track identity comparison, shared by the library index (package main) and by
// Deezer translation (here).
//
// It lives in backend because both packages need the same answer to "are these
// the same recording?". Two implementations would drift, and the consequences of
// drift are asymmetric: the library side decides whether to offer a download,
// while the Deezer side decides which file gets downloaded under a given name.

var (
	reParen = regexp.MustCompile(`[\(\[][^\)\]]*[\)\]]`)
	reFeat  = regexp.MustCompile(`(?i)\b(feat|ft|featuring|with)\b.*$`)
	// Spotify writes the same version qualifier two ways — `Vaa Vaathi (From
	// "Vaathi")` on the album, `Vaa Vaathi - From "Vaathi"` on the single — and
	// Deezer splits it into title_version. All three must reduce alike.
	reDashSuffix = regexp.MustCompile(`\s+[-–—]\s+.*$`)
	// Every separator seen between artist names across the sources that have to
	// agree: SpotiFLAC's tagger ("•"), Spotify (", "), Deezer's contributors,
	// and whatever a file scanned from disk carries.
	reArtistSep = regexp.MustCompile(`(?i)\s*(?:[•·,;/&|]|\bx\b|\bfeat\.?\b|\bft\.?\b|\bfeaturing\b|\bwith\b)\s*`)
)

// NormalizeTitle reduces a title to comparable letters, dropping version
// qualifiers in either spelling.
func NormalizeTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = reParen.ReplaceAllString(s, "")
	s = reDashSuffix.ReplaceAllString(s, "")
	s = reFeat.ReplaceAllString(s, "")
	return KeepAlphanumeric(s)
}

func KeepAlphanumeric(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ArtistTokens splits an artist credit into the individual performers.
func ArtistTokens(artist string) []string {
	out := make([]string, 0, 4)
	for _, part := range reArtistSep.Split(strings.ToLower(artist), -1) {
		if t := KeepAlphanumeric(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ArtistsOverlap reports whether two credits name a performer in common.
//
// Set overlap, not equality: a film song is credited "G. V. Prakash Kumar • Sid
// Sriram" by SpotiFLAC's tagger, "Sid Sriram, G. V. Prakash Kumar" by Spotify
// and "A. R. Rahman, Minmini" by Deezer's contributors. Neither the order nor
// the number of names listed is identity.
func ArtistsOverlap(a, b string) bool {
	ta, tb := ArtistTokens(a), ArtistTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return true // unknown on one side; the title match stands alone
	}
	for _, x := range ta {
		for _, y := range tb {
			if x == y {
				return true
			}
			// "G. V. Prakash" vs "G. V. Prakash Kumar": one credit carries the
			// fuller name. Require a real prefix and some length so short names
			// can't swallow each other.
			if len(x) >= 5 && len(y) >= 5 && (strings.HasPrefix(x, y) || strings.HasPrefix(y, x)) {
				return true
			}
		}
	}
	fa, fb := KeepAlphanumeric(strings.ToLower(a)), KeepAlphanumeric(strings.ToLower(b))
	return fa != "" && fb != "" && (strings.Contains(fa, fb) || strings.Contains(fb, fa))
}

// FirstCredit returns the first named performer, still readable — original
// spacing and casing — so it can go into a search query.
//
// ArtistTokens is the wrong tool for that: it strips everything but letters and
// digits, which is right for comparison and useless as a query term. And
// GetFirstArtist splits on ";" and "feat" but not commas, so a Spotify credit
// like "Minmini, A.R. Rahman, Vairamuthu" survives whole and matches nothing.
func FirstCredit(artist string) string {
	parts := reArtistSep.Split(strings.TrimSpace(artist), -1)
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			return p
		}
	}
	return strings.TrimSpace(artist)
}

// CreditsMatch is ArtistsOverlap with the "unknown artist" hole closed: an
// untagged record must not match every credit. That leniency is safe where one
// targeted lookup bounds the candidates, and unsafe over a whole catalog.
func CreditsMatch(have, want string) bool {
	if len(ArtistTokens(have)) == 0 && len(ArtistTokens(want)) > 0 {
		return false
	}
	return ArtistsOverlap(have, want)
}
