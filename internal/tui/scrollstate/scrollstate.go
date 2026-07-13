package scrollstate

// IsUserScrolledUp returns whether the viewport is above the bottom.
//
// yOffset is the current top visible line index.
// visibleLineCount is the total rendered line count in content.
// viewportHeight is the number of lines the viewport can display.
func IsUserScrolledUp(yOffset, visibleLineCount, viewportHeight int) bool {
	maxOffset := visibleLineCount - viewportHeight
	if maxOffset < 0 {
		maxOffset = 0
	}
	return yOffset < maxOffset
}
