package compare

import (
	"strings"
	"testing"
)

// A made-up token, assembled here so no token-shaped literal sits in the source.
var token = "ey" + "J" + strings.Repeat("a", 20) + "." + strings.Repeat("b", 20) + "." + strings.Repeat("c", 20)

func TestPromptRedactsTokensInRecords(t *testing.T) {
	p := Prompt(Alert{Name: "a", Where: "1=1"}, nil, []Row{{Record: "auth failed for " + token + " on /login", Count: 3}})
	if strings.Contains(p, token) || strings.Contains(p, "aaaa") {
		t.Fatalf("token reached the prompt: %s", p)
	}
	if !strings.Contains(p, "auth failed for "+Redacted+" on /login") {
		t.Fatalf("the record around the token must survive: %s", p)
	}
}

func TestPromptRedactsTokensInIncidents(t *testing.T) {
	inc := map[string]any{"status": map[string]any{
		"rootCause": map[string]any{"statement": "bearer " + token + " leaked"},
		"howToFix":  map[string]any{"apply": "curl -H \"Authorization: Bearer " + token + "\" x"}}}
	if p := Prompt(Alert{Name: "a"}, []map[string]any{inc}, nil); strings.Contains(p, token) {
		t.Fatalf("token reached the prompt: %s", p)
	}
}

// The limit must not leave the head of a token behind: redaction comes first.
func TestCutRedactsBeforeTruncating(t *testing.T) {
	got := cut(strings.Repeat("x", 10)+token, 15)
	if strings.Contains(got, "eyJ") {
		t.Fatalf("a token head survived the cut: %q", got)
	}
}

func TestShortStringsAreNotTokens(t *testing.T) {
	if got := cut("eyJshort", 100); got != "eyJshort" {
		t.Fatalf("got %q", got)
	}
}
