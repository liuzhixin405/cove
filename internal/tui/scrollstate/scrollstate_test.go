package scrollstate

import "testing"

func TestIsUserScrolledUp(t *testing.T) {
	tests := []struct {
		name       string
		yOffset    int
		visible    int
		height     int
		wantScroll bool
	}{
		{name: "at bottom", yOffset: 90, visible: 100, height: 10, wantScroll: false},
		{name: "above bottom", yOffset: 80, visible: 100, height: 10, wantScroll: true},
		{name: "content shorter than viewport", yOffset: 0, visible: 5, height: 10, wantScroll: false},
		{name: "negative effective max clamps", yOffset: 0, visible: 0, height: 10, wantScroll: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsUserScrolledUp(tt.yOffset, tt.visible, tt.height)
			if got != tt.wantScroll {
				t.Fatalf("IsUserScrolledUp(%d, %d, %d)=%v, want %v", tt.yOffset, tt.visible, tt.height, got, tt.wantScroll)
			}
		})
	}
}
