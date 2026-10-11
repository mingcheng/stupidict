package dictionary

import (
	"context"
	"sort"
	"unicode"
	"unicode/utf16"
)

func isSpace(character rune) bool { return unicode.IsSpace(character) }

func IsChineseQuery(text string) bool {
	for _, character := range text {
		if character == 0x3007 || character >= 0x3400 && character <= 0x9fff ||
			character >= 0xf900 && character <= 0xfaff || character >= 0x20000 && character <= 0x2a6df ||
			character >= 0x2a700 && character <= 0x2b73f || character >= 0x2b740 && character <= 0x2b81f ||
			character >= 0x2b820 && character <= 0x2ceaf || character >= 0x2ceb0 && character <= 0x2ee5f ||
			character >= 0x2f800 && character <= 0x2fa1f || character >= 0x30000 && character <= 0x323af ||
			character >= 0x323b0 && character <= 0x3347f {
			return true
		}
	}
	return false
}

func utf16Length(text string) int { return len(utf16.Encode([]rune(text))) }

func ordinalLess(left, right string) bool {
	leftUnits, rightUnits := utf16.Encode([]rune(left)), utf16.Encode([]rune(right))
	for index := 0; index < min(len(leftUnits), len(rightUnits)); index++ {
		if leftUnits[index] != rightUnits[index] {
			return leftUnits[index] < rightUnits[index]
		}
	}
	return len(leftUnits) < len(rightUnits)
}

type commonWord struct {
	word  string
	units []uint16
	rank  int
}

func rank(frequency, bnc int) int {
	if frequency > 0 {
		return frequency
	}
	if bnc > 0 {
		return bnc
	}
	return 999999
}

func (dictionary *Dictionary) commonWords(ctx context.Context) ([]commonWord, error) {
	dictionary.commonGate.Lock()
	defer dictionary.commonGate.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dictionary.common != nil {
		return dictionary.common, nil
	}
	rows, err := dictionary.database.QueryContext(ctx, "SELECT word_lower, freq, bnc FROM word WHERE freq > 0 OR bnc > 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	words := []commonWord{}
	for rows.Next() {
		var word string
		var frequency, bnc int
		if err := rows.Scan(&word, &frequency, &bnc); err != nil {
			return nil, err
		}
		words = append(words, commonWord{word, utf16.Encode([]rune(word)), rank(frequency, bnc)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(words, func(left, right int) bool {
		if words[left].rank != words[right].rank {
			return words[left].rank < words[right].rank
		}
		return ordinalLess(words[left].word, words[right].word)
	})
	dictionary.common = words
	return words, nil
}

func fuzzyWords(ctx context.Context, query string, candidates []commonWord, limit int) ([]string, error) {
	units := utf16.Encode([]rune(query))
	cutoff := 3
	if len(units) <= 4 {
		cutoff = 1
	} else if len(units) <= 8 {
		cutoff = 2
	}
	type match struct {
		word           string
		distance, rank int
	}
	matches := []match{}
	for index, candidate := range candidates {
		if index%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if candidate.word == query || len(candidate.units)-len(units) > cutoff || len(units)-len(candidate.units) > cutoff {
			continue
		}
		distance := boundedDistance(units, candidate.units, cutoff)
		if distance >= 0 {
			matches = append(matches, match{candidate.word, distance, candidate.rank})
		}
	}
	sort.Slice(matches, func(left, right int) bool {
		if matches[left].distance != matches[right].distance {
			return matches[left].distance < matches[right].distance
		}
		if matches[left].rank != matches[right].rank {
			return matches[left].rank < matches[right].rank
		}
		return ordinalLess(matches[left].word, matches[right].word)
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	words := make([]string, 0, len(matches))
	for _, match := range matches {
		words = append(words, match.word)
	}
	return words, nil
}

func boundedDistance(left, right []uint16, cutoff int) int {
	previousPrevious, previous, current := make([]int, len(right)+1), make([]int, len(right)+1), make([]int, len(right)+1)
	for index := range previous {
		previous[index] = index
	}
	for row := 1; row <= len(left); row++ {
		current[0] = row
		rowMin := row
		for column := 1; column <= len(right); column++ {
			cost := 1
			if left[row-1] == right[column-1] {
				cost = 0
			}
			value := min(current[column-1]+1, previous[column]+1, previous[column-1]+cost)
			if row > 1 && column > 1 && left[row-2] == right[column-1] && left[row-1] == right[column-2] {
				value = min(value, previousPrevious[column-2]+1)
			}
			current[column] = value
			rowMin = min(rowMin, value)
		}
		if rowMin > cutoff {
			return -1
		}
		previousPrevious, previous, current = previous, current, previousPrevious
	}
	if previous[len(right)] <= cutoff {
		return previous[len(right)]
	}
	return -1
}
