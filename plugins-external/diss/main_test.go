package main

import "testing"

func TestClean(t *testing.T) {
	if s, err := clean("\ufeff  你好 \n"); err != nil || s != "你好" {
		t.Fatalf("got %q %v", s, err)
	}
	for _, in := range []string{"", "  \n", "<html>502</html>"} {
		if _, err := clean(in); err == nil {
			t.Fatalf("%q should fail", in)
		}
	}
}
