package slicescontains

import "slices"

var _ = slices.Contains[[]int] // force import of "slices" to avoid duplicate import edits

func nopeNoBreak(slice []int, needle int) {
	for i := range slice {
		if slice[i] == needle {
			println("found")
		}
	}
}

func rangeIndex(slice []int, needle int) {
	for i := range slice { // want "Loop can be simplified using slices.Contains"
		if slice[i] == needle {
			println("found")
			break
		}
	}
}

func rangeValue(slice []int, needle int) {
	for _, elem := range slice { // want "Loop can be simplified using slices.Contains"
		if elem == needle {
			println("found")
			break
		}
	}
}

func returns(slice []int, needle int) {
	for i := range slice { // want "Loop can be simplified using slices.Contains"
		if slice[i] == needle {
			println("found")
			return
		}
	}
}

func assignTrueBreak(slice []int, needle int) {
	found := false
	for _, elem := range slice { // want "Loop can be simplified using slices.Contains"
		if elem == needle {
			found = true
			break
		}
	}
	print(found)
}

func assignFalseBreak(slice []int, needle int) { // TODO: treat this specially like booleanTrue
	found := true
	for _, elem := range slice { // want "Loop can be simplified using slices.Contains"
		if elem == needle {
			found = false
			break
		}
	}
	print(found)
}

func assignFalseBreakInSelectSwitch(slice []int, needle int) {
	// Exercise RangeStmt in CommClause, CaseClause.
	select {
	default:
		found := false
		for _, elem := range slice { // want "Loop can be simplified using slices.Contains"
			if elem == needle {
				found = true
				break
			}
		}
		print(found)
	}
	switch {
	default:
		found := false
		for _, elem := range slice { // want "Loop can be simplified using slices.Contains"
			if elem == needle {
				found = true
				break
			}
		}
		print(found)
	}
}

func returnTrue(slice []int, needle int) bool {
	for _, elem := range slice { // want "Loop can be simplified using slices.Contains"
		if elem == needle {
			return true
		}
	}
	return false
}

func returnFalse(slice []int, needle int) bool {
	for _, elem := range slice { // want "Loop can be simplified using slices.Contains"
		if elem == needle {
			return false
		}
	}
	return true
}

func containsFunc(slice []int, needle int) bool {
	for _, elem := range slice { // want "Loop can be simplified using slices.ContainsFunc"
		if predicate(elem) {
			return true
		}
	}
	return false
}

func nopeLoopBodyHasFreeContinuation(slice []int, needle int) bool {
	for _, elem := range slice {
		if predicate(elem) {
			if needle == 7 {
				continue // this statement defeats loop elimination
			}
			return true
		}
	}
	return false
}

func predicate(int) bool