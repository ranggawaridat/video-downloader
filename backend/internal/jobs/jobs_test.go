package jobs

import "testing"

func TestValidFormat(t *testing.T) {
	tests := []struct {
		format string
		valid  bool
	}{
		{format: "audio", valid: true},
		{format: "1024", valid: true},
		{format: "999", valid: true},
		{format: "0", valid: false},
		{format: "1024p", valid: false},
		{format: "+1024", valid: false},
		{format: "", valid: false},
	}

	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			if got := validFormat(tt.format); got != tt.valid {
				t.Errorf("validFormat(%q) = %t, want %t", tt.format, got, tt.valid)
			}
		})
	}
}
