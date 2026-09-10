package notify

import "testing"

// The push gate is where the filter's decision could be quietly undone. An
// issue that was already assigned when dibs scored it was surfaced on purpose,
// so only a name that arrived afterwards means somebody took it.
func TestNewlyAssigned(t *testing.T) {
	const self = "test-user"
	tests := []struct {
		name    string
		current []string
		known   []string
		want    bool
	}{
		{"nobody has it", nil, nil, false},
		{"assigned since dibs looked", []string{"rando"}, nil, true},
		{"assigned all along", []string{"maintainer"}, []string{"maintainer"}, false},
		{"a second name arrived", []string{"maintainer", "rando"}, []string{"maintainer"}, true},
		{"the operator took it", []string{self}, nil, false},
		{"the operator match ignores case", []string{"Test-User"}, nil, false},
		{"the known match ignores case", []string{"Maintainer"}, []string{"maintainer"}, false},
		{"the assignment was dropped", nil, []string{"maintainer"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newlyAssigned(tt.current, tt.known, self); got != tt.want {
				t.Errorf("newlyAssigned(%v, %v) = %v, want %v", tt.current, tt.known, got, tt.want)
			}
		})
	}
}
