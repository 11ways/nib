package tag

import "testing"

// Vectors produced by the TypeScript implementation (Bun.hash.xxHash32), which oh-my-pi's file_hash matches.
func TestOfMatchesKnownVectors(t *testing.T) {
	cases := map[string]string{
		"":                                      "5D05",
		"one\ntwo\nthree\n":                     "2543",
		"one\r\ntwo\r\nthree":                   "9932",
		"a  \t\nb\r\n":                          "9A46",
		"h\u00e9llo w\u00f6rld \U0001F600\n":    "2B5B",
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx": "11F5",
		"0123456789abcdef0123456789abcdef!":     "A739",
		"\uFEFFbom\n":                           "50BF",
	}
	for input, want := range cases {
		if got := Of(input); got != want {
			t.Errorf("Of(%q) = %s, want %s", input, got, want)
		}
	}
}
