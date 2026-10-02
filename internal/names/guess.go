package names

import (
	"strings"
	"sync"
	"time"

	"github.com/ikawaha/kagome-dict/ipa"
	"github.com/ikawaha/kagome/v2/tokenizer"
)

// Kanji readings are guessed with a morphological dictionary. Readings of
// names are often wrong, so a guess is only ever used for search and weak
// hints, never shown and never used to link anything.

// The dictionary takes about 90 MB of memory, so it's loaded on first use
// and dropped again after a while without guesses. Guesses are only needed
// when names are added.
var (
	guessMu    sync.Mutex
	guesser    *tokenizer.Tokenizer
	guessTimer *time.Timer
)

const guessIdle = 5 * time.Minute

func loadGuesser() *tokenizer.Tokenizer {
	guessMu.Lock()
	defer guessMu.Unlock()
	if guesser == nil {
		t, err := tokenizer.New(ipa.Dict(), tokenizer.OmitBosEos())
		if err != nil {
			return nil
		}
		guesser = t
	}
	if guessTimer == nil {
		guessTimer = time.AfterFunc(guessIdle, func() {
			guessMu.Lock()
			guesser, guessTimer = nil, nil
			guessMu.Unlock()
		})
	} else {
		guessTimer.Reset(guessIdle)
	}
	return guesser
}

// ipaReading is where the IPA dictionary keeps a word's reading.
const ipaReading = 7

// GuessKey is a guessed romanization of a name with kanji, for search and
// weak hints only. It's empty for names without kanji, which RomajiKey
// covers exactly, and for names it can't read.
func GuessKey(s string) string {
	if !hasHan(s) {
		return ""
	}
	g := loadGuesser()
	if g == nil {
		return ""
	}
	var b strings.Builder
	for _, tok := range g.Tokenize(s) {
		if f := tok.Features(); len(f) > ipaReading && f[ipaReading] != "*" {
			b.WriteString(f[ipaReading])
			continue
		}
		if hasHan(tok.Surface) {
			return "" // a kanji word it doesn't know
		}
		b.WriteString(tok.Surface)
	}
	return FoldLongVowels(MatchKey(Romaji(b.String())))
}
