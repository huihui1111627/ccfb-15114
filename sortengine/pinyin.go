package sortengine

import (
	"sort"
)

// PinyinCode returns a 32-bit ordering code for the Han character r.
//
// The code is syllable*5 + tone, where the syllable is the toneless Mandarin
// reading from Unicode Unihan kMandarin and tone is 1..4 (0 for the handful of
// neutral-tone readings). Because syllables are numbered in string order,
// comparing codes compares readings the way a pinyin dictionary does: first
// alphabetically by syllable, then by tone.
//
// Characters with no reading keep their code point shifted into a separate
// range so that they still have a deterministic, stable order with respect to
// each other and never collide with a reading. For such characters the code is
// pinyinNoReading | rune, with pinyinNoReading well above any real
// syllable*5+4 value (418 syllables * 5 = 2090).
func PinyinCode(r rune) uint32 {
	if v, ok := pinyinLookup(r); ok {
		return v
	}
	return pinyinNoReading | uint32(r)
}

const pinyinNoReading = 0x01000000

func pinyinLookup(r rune) (uint32, bool) {
	if r >= pinyinDenseLo && r < pinyinDenseLo+rune(len(pinyinDense)) {
		v := pinyinDense[r-pinyinDenseLo]
		if v != 0 {
			return uint32(v), true
		}
		return 0, false
	}
	i := sort.Search(len(pinyinExtKeys), func(i int) bool { return pinyinExtKeys[i] >= r })
	if i < len(pinyinExtKeys) && pinyinExtKeys[i] == r {
		return uint32(pinyinExtVals[i]), true
	}
	return 0, false
}

// PinyinString converts s to a sequence of ordering codes, one per rune.
func PinyinString(s string, dst []uint32) []uint32 {
	for _, r := range s {
		dst = append(dst, PinyinCode(r))
	}
	return dst
}

// pinyinRuneCode is the encoder's entry point. Every rune — Han, Latin, or
// anything else — gets a PinyinCode, so a name that mixes scripts still has a
// single, total order: Han by reading first, then everything else by code
// point.
func pinyinRuneCode(r rune) uint32 {
	return PinyinCode(r)
}
