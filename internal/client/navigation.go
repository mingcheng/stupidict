package client

import dictionary "github.com/mingcheng/stupidict"

// Navigation is confined to the frontend event loop. Pages are immutable after
// insertion, so back/forward preserves the complete result without another read.
type Navigation struct {
	pages    []dictionary.LookupResult
	position int
}

func (navigation *Navigation) Push(result dictionary.LookupResult) {
	if len(navigation.pages) > 0 {
		if navigation.pages[navigation.position].Query == result.Query {
			return
		}
		navigation.pages = navigation.pages[:navigation.position+1]
	}
	navigation.pages = append(navigation.pages, result)
	// Bound session memory independently of the persistent recent-query list.
	if len(navigation.pages) > 100 {
		navigation.pages = navigation.pages[1:]
	}
	navigation.position = len(navigation.pages) - 1
}

func (navigation *Navigation) Move(delta int) (dictionary.LookupResult, bool) {
	position := navigation.position + delta
	if position < 0 || position >= len(navigation.pages) {
		return dictionary.LookupResult{}, false
	}
	navigation.position = position
	return navigation.pages[position], true
}

func (navigation *Navigation) CanBack() bool {
	return len(navigation.pages) > 0 && navigation.position > 0
}
func (navigation *Navigation) CanForward() bool { return navigation.position+1 < len(navigation.pages) }
