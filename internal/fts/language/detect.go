package language

import "github.com/abadojack/whatlanggo"

// detectLanguage restricts detection to candidates; ok=false on a sample under
// minRunes or an unreliable result, and callers fall back to the first language.
func detectLanguage(sample string, candidates []string, minRunes int) (lang string, ok bool) {
	if len([]rune(sample)) < minRunes {
		return "", false
	}
	whitelist := make(map[whatlanggo.Lang]bool, len(candidates))
	for _, c := range candidates {
		if wl, found := isoToWhatlang[c]; found {
			whitelist[wl] = true
		}
	}
	if len(whitelist) == 0 {
		return "", false
	}
	info := whatlanggo.DetectWithOptions(sample, whatlanggo.Options{Whitelist: whitelist})
	if !info.IsReliable() {
		return "", false
	}
	code := info.Lang.Iso6391()
	for _, c := range candidates {
		if c == code {
			return code, true
		}
	}
	return "", false
}

// isoToWhatlang maps the ISO 639-1 codes this package can build a filter
// chain for. uk has no Snowball stemmer but is still detectable
// (lowercase+stopwords chain), which keeps uk parts from being mis-stemmed
// under ru in a mixed mailbox.
var isoToWhatlang = map[string]whatlanggo.Lang{
	"en": whatlanggo.Eng,
	"fr": whatlanggo.Fra,
	"de": whatlanggo.Deu,
	"it": whatlanggo.Ita,
	"pt": whatlanggo.Por,
	"ru": whatlanggo.Rus,
	"es": whatlanggo.Spa,
	"uk": whatlanggo.Ukr,
}
