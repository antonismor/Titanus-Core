package unitruntime

import "testing"

func TestParseBytes(t *testing.T) {
	tests := map[string]int64{
		"512M":   512 * 1024 * 1024,
		"1GiB":   1024 * 1024 * 1024,
		"2GB":    2 * 1000 * 1000 * 1000,
		"1048576": 1048576,
	}
	for input, expected := range tests {
		got, err := ParseBytes(input)
		if err != nil {
			t.Fatalf("ParseBytes(%q): %v", input, err)
		}
		if got != expected {
			t.Fatalf("ParseBytes(%q)=%d, want %d", input, got, expected)
		}
	}
}

func TestSpecValidation(t *testing.T) {
	s := Spec{ID: "web01", Source: "debian13", Command: []string{"/bin/sh"}}
	s.Normalize()
	if err := s.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
}
