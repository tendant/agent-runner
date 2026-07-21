package textutil

import "testing"

func TestTruncate(t *testing.T) {
	tests := []struct {
		s    string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly-10", 10, "exactly-10"},
		{"this is longer", 7, "this is..."},
		{"", 5, ""},
		{"anything", 0, ""},
		{"你好世界你好世界", 4, "你好世界..."},
	}
	for _, tt := range tests {
		if got := Truncate(tt.s, tt.n); got != tt.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{`{"a":1}`, `{"a":1}`, true},
		{"  {\"a\":1}\n", `{"a":1}`, true},
		{"prose before {\"a\":1} prose after", `{"a":1}`, true},
		{"no json here", "", false},
		{"only open {", "", false},
		{"} reversed {", "", false},
	}
	for _, tt := range tests {
		got, ok := ExtractJSON(tt.in)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("ExtractJSON(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}
