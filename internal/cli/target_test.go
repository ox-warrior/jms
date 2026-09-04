package cli

import (
	"testing"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

func TestParseShortcut(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    shortcutTarget
	}{
		{"kind and name", "ec2 ng0", shortcutTarget{Kind: jmsd.ResourceEC2, Name: "ng0"}},
		{"kind only", "ec2", shortcutTarget{Kind: jmsd.ResourceEC2}},
		{"kubernetes alias", "kubernetes prod", shortcutTarget{Kind: jmsd.ResourceK8s, Name: "prod"}},
		{"case-insensitive kind", "K8S prod", shortcutTarget{Kind: jmsd.ResourceK8s, Name: "prod"}},
		// mfmc's own preprocessing collapses runs of whitespace before
		// splitting -- strings.Fields does this for free.
		{"extra whitespace collapses", "  ec2    ng0  ", shortcutTarget{Kind: jmsd.ResourceEC2, Name: "ng0"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseShortcut(c.command)
			if err != nil {
				t.Fatalf("parseShortcut(%q) error = %v, want nil", c.command, err)
			}
			if got != c.want {
				t.Errorf("parseShortcut(%q) = %+v, want %+v", c.command, got, c.want)
			}
		})
	}
}

func TestParseShortcutRejectsUnknownType(t *testing.T) {
	_, err := parseShortcut("ec3 ng0")
	if err == nil {
		t.Fatal("parseShortcut(\"ec3 ng0\") error = nil, want an error")
	}
}

func TestParseShortcutRejectsWrongFieldCount(t *testing.T) {
	cases := []string{"", "ec2 ng0 root", "  "}
	for _, command := range cases {
		if _, err := parseShortcut(command); err == nil {
			t.Errorf("parseShortcut(%q) error = nil, want an error -- mfmc's own syntax is exactly 1 or 2 fields", command)
		}
	}
}
