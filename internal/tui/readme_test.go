package tui

import (
	"os"
	"strings"
	"testing"
)

// TestREADMEKeys checks that README.md's key table is helpKeys row for row, so the README
// and the help overlay cannot drift apart (DEV-65).
func TestREADMEKeys(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	esc := strings.NewReplacer("<", "&lt;", ">", "&gt;", "|", `\|`)
	var want strings.Builder
	want.WriteString("| Key | Action |\n| --- | --- |\n")
	for _, k := range helpKeys {
		want.WriteString("| `" + k[0] + "` | " + esc.Replace(k[1]) + " |\n")
	}
	if !strings.Contains(string(b), want.String()) {
		t.Errorf("README.md has no key table matching helpKeys; want\n%s", want.String())
	}
}
