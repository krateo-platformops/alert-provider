package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/krateo-platformops/alert-provider/internal/golden"
	"github.com/krateo-platformops/alert-provider/internal/pyfmt"
)

// parseRootCause is the rootCause ParseFix keeps from an answer whose block holds rc, and the
// coercion notes it returns.
func parseRootCause(t *testing.T, rc map[string]any) (map[string]any, []string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"rootCause": rc})
	if err != nil {
		t.Fatal(err)
	}
	_, v2, _, notes := ParseFix("## Root cause\nprose\n\n```json\n"+string(b)+"\n```", nil)
	got, _ := v2["rootCause"].(map[string]any)
	return got, notes
}

func wantRootCause(t *testing.T, got, want map[string]any) {
	t.Helper()
	if d := golden.Diff(want, got); d != "" {
		t.Errorf("rootCause (-want +got):\n%s", d)
	}
}

func wantNotes(t *testing.T, notes []string, want ...string) {
	t.Helper()
	if len(notes) != len(want) {
		t.Fatalf("notes %q, want %d matching %q", notes, len(want), want)
	}
	for i, w := range want {
		if !strings.Contains(notes[i], w) {
			t.Errorf("note %d %q does not contain %q", i, notes[i], w)
		}
	}
}

func TestARootCauseThatFitsTheCRDIsKeptAsGiven(t *testing.T) {
	ev := []any{
		map[string]any{"kind": "component", "ref": "apps/Deployment/krateo-system/snowplow", "note": "controller shipped by Krateo"},
		map[string]any{"kind": "trace", "ref": "4bf92f3577b34da6a3ce929d0e0e4736"},
	}
	got, notes := parseRootCause(t, map[string]any{"statement": "snowplow caches a stale RBAC view",
		"confidence": 0.2, "category": "permission", "owner": "krateo", "ownerEvidence": ev})
	wantRootCause(t, got, map[string]any{"statement": "snowplow caches a stale RBAC view", "confidence": "0.2", // under the cap of an answer with no retrieval (policy.go)
		"category": "permission", "owner": "krateo", "ownerEvidence": ev})
	wantNotes(t, notes)
}

func TestEveryCategoryAndOwnerOfTheCRDIsKept(t *testing.T) {
	for _, c := range categories {
		got, notes := parseRootCause(t, map[string]any{"statement": "x", "category": c, "owner": "customer"})
		if got["category"] != c || got["owner"] != "customer" || len(notes) != 0 {
			t.Errorf("category %s: %v, notes %q", c, got, notes)
		}
	}
	for _, o := range owners {
		got, notes := parseRootCause(t, map[string]any{"statement": "x", "owner": o})
		if got["owner"] != o || len(notes) != 0 {
			t.Errorf("owner %s: %v, notes %q", o, got, notes)
		}
	}
}

func TestACategoryOutsideTheEnumIsWrittenAsOther(t *testing.T) {
	got, notes := parseRootCause(t, map[string]any{"statement": "x", "category": "rbac", "owner": "krateo"})
	wantRootCause(t, got, map[string]any{"statement": "x", "category": "other", "owner": "krateo"})
	wantNotes(t, notes, "rootCause.category 'rbac' is not one of config, capacity, image, network, dependency, permission, other, written as other")

	// Not a string: still a value the CRD would refuse.
	got, notes = parseRootCause(t, map[string]any{"statement": "x", "category": 3, "owner": "krateo"})
	wantRootCause(t, got, map[string]any{"statement": "x", "category": "other", "owner": "krateo"})
	wantNotes(t, notes, "rootCause.category 3 is not one of")
}

func TestACategoryInAnotherCaseIsLowercasedNotCoerced(t *testing.T) {
	got, notes := parseRootCause(t, map[string]any{"statement": "x", "category": " Image ", "owner": "Krateo"})
	wantRootCause(t, got, map[string]any{"statement": "x", "category": "image", "owner": "krateo"})
	wantNotes(t, notes)
}

func TestAMissingCategoryStaysMissing(t *testing.T) {
	got, notes := parseRootCause(t, map[string]any{"statement": "x", "owner": "unknown"})
	wantRootCause(t, got, map[string]any{"statement": "x", "owner": "unknown"})
	wantNotes(t, notes)
}

func TestAMissingOwnerIsWrittenAsUnknown(t *testing.T) {
	for _, rc := range []map[string]any{{"statement": "x"}, {"statement": "x", "owner": nil}} {
		got, notes := parseRootCause(t, rc)
		wantRootCause(t, got, map[string]any{"statement": "x", "owner": "unknown"})
		wantNotes(t, notes, "rootCause.owner missing, written as unknown")
	}
}

func TestAnOwnerOutsideTheEnumIsWrittenAsUnknown(t *testing.T) {
	for _, c := range []struct {
		owner any
		note  string
	}{
		{"vendor", "rootCause.owner 'vendor' is not one of customer, krateo, unknown, written as unknown"},
		{"", "rootCause.owner '' is not one of"},
		{true, "rootCause.owner True is not one of"},
		{map[string]any{"who": "krateo"}, "rootCause.owner {'who': 'krateo'} is not one of"},
	} {
		got, notes := parseRootCause(t, map[string]any{"statement": "x", "owner": c.owner})
		wantRootCause(t, got, map[string]any{"statement": "x", "owner": "unknown"})
		wantNotes(t, notes, c.note)
	}
}

// A long model-written value is quoted cut, never whole.
func TestACoercedValueIsQuotedCut(t *testing.T) {
	_, notes := parseRootCause(t, map[string]any{"statement": "x", "owner": strings.Repeat("y", 5000)})
	wantNotes(t, notes, "'"+strings.Repeat("y", shownMax-1)+"…")
	if len(notes[0]) > 200 {
		t.Errorf("note is %d bytes: %q", len(notes[0]), notes[0])
	}
}

func TestOwnerEvidenceWithABadKindOrRefIsDropped(t *testing.T) {
	good := map[string]any{"kind": "config", "ref": "composition.krateo.io/FireworksApp/team-a/web"}
	got, notes := parseRootCause(t, map[string]any{"statement": "x", "owner": "customer", "ownerEvidence": []any{
		map[string]any{"kind": "chart", "ref": "a/b/c/d"},
		map[string]any{"ref": "a/b/c/d"},
		map[string]any{"kind": "log", "ref": ""},
		map[string]any{"kind": "log", "ref": " \t "},
		map[string]any{"kind": "log"},
		map[string]any{"kind": "log", "ref": strings.Repeat("r", ownerEvidenceRefMax+1)},
		"apps/Deployment/x/y",
		good,
	}})
	wantRootCause(t, got, map[string]any{"statement": "x", "owner": "customer", "ownerEvidence": []any{good}})
	wantNotes(t, notes,
		"rootCause.ownerEvidence[0] dropped: kind 'chart' is not one of component, config, trace, log",
		"rootCause.ownerEvidence[1] dropped: kind None is not one of",
		"rootCause.ownerEvidence[2] dropped: no ref",
		"rootCause.ownerEvidence[3] dropped: no ref",
		"rootCause.ownerEvidence[4] dropped: no ref",
		"rootCause.ownerEvidence[5] dropped: ref over 512 characters",
		"rootCause.ownerEvidence[6] dropped: not an object")
}

// The CRD's bounds count characters: 512 multi-byte characters are a valid ref.
func TestARefAtTheLimitInMultiByteCharactersIsKept(t *testing.T) {
	ref := strings.Repeat("é", ownerEvidenceRefMax)
	got, notes := parseRootCause(t, map[string]any{"statement": "x", "owner": "krateo",
		"ownerEvidence": []any{map[string]any{"kind": "component", "ref": ref}}})
	wantRootCause(t, got, map[string]any{"statement": "x", "owner": "krateo",
		"ownerEvidence": []any{map[string]any{"kind": "component", "ref": ref}}})
	wantNotes(t, notes)
}

func TestALongNoteIsCutTo300CharactersOnACharacterBoundary(t *testing.T) {
	note := strings.Repeat("ü", ownerEvidenceNoteMax-1) + "🙂🙂"
	got, notes := parseRootCause(t, map[string]any{"statement": "x", "owner": "krateo",
		"ownerEvidence": []any{map[string]any{"kind": "component", "ref": "apps/Deployment/a/b", "note": note}}})
	cut := got["ownerEvidence"].([]map[string]any)[0]["note"].(string)
	if pyfmt.Len(cut) != ownerEvidenceNoteMax || !utf8.ValidString(cut) || !strings.HasSuffix(cut, "ü🙂") {
		t.Errorf("note %d characters, valid UTF-8 %v: %q", pyfmt.Len(cut), utf8.ValidString(cut), cut)
	}
	wantNotes(t, notes, "rootCause.ownerEvidence[0].note cut to 300 characters")

	// At the limit: kept whole, nothing to say.
	note = strings.Repeat("ü", ownerEvidenceNoteMax)
	got, notes = parseRootCause(t, map[string]any{"statement": "x", "owner": "krateo",
		"ownerEvidence": []any{map[string]any{"kind": "component", "ref": "apps/Deployment/a/b", "note": note}}})
	if kept := got["ownerEvidence"].([]map[string]any)[0]["note"]; kept != note {
		t.Errorf("note %q", kept)
	}
	wantNotes(t, notes)
}

func TestOwnerEvidenceKeepsAtMost8Items(t *testing.T) {
	var ev []any
	for i := range 11 {
		ev = append(ev, map[string]any{"kind": "log", "ref": fmt.Sprintf("/Pod/team-a/web-%d", i)})
	}
	// A bad item does not count towards the 8.
	ev = append([]any{map[string]any{"kind": "nope", "ref": "x"}}, ev...)
	got, notes := parseRootCause(t, map[string]any{"statement": "x", "owner": "customer", "ownerEvidence": ev})
	wantRootCause(t, got, map[string]any{"statement": "x", "owner": "customer", "ownerEvidence": ev[1:9]})
	wantNotes(t, notes, "ownerEvidence[0] dropped", "rootCause.ownerEvidence: 3 items after the first 8 kept dropped")
}

func TestOwnerEvidenceThatIsNotAListIsDropped(t *testing.T) {
	got, notes := parseRootCause(t, map[string]any{"statement": "x", "owner": "krateo",
		"ownerEvidence": map[string]any{"kind": "component", "ref": "a/b/c/d"}})
	wantRootCause(t, got, map[string]any{"statement": "x", "owner": "krateo"})
	wantNotes(t, notes, "rootCause.ownerEvidence is not a list, dropped")

	// Empty, or every item dropped: no ownerEvidence at all, never an empty list.
	got, _ = parseRootCause(t, map[string]any{"statement": "x", "owner": "krateo", "ownerEvidence": []any{}})
	wantRootCause(t, got, map[string]any{"statement": "x", "owner": "krateo"})
}

// The notes go to the log: they quote the coerced value, never the refs, notes or statement.
func TestTheNotesQuoteNoOtherModelText(t *testing.T) {
	secret := strings.Join([]string{"sentinel", "do", "not", "log"}, "-") // built at run time
	_, notes := parseRootCause(t, map[string]any{"statement": secret, "category": "bogus", "owner": "nobody",
		"ownerEvidence": []any{
			map[string]any{"kind": "log", "ref": secret + strings.Repeat("r", ownerEvidenceRefMax)},
			map[string]any{"kind": "log", "ref": "/Pod/a/b", "note": secret + strings.Repeat("n", ownerEvidenceNoteMax)},
			map[string]any{"kind": "bad", "ref": secret},
		}})
	if len(notes) != 5 {
		t.Fatalf("notes %q", notes)
	}
	for _, n := range notes {
		if strings.Contains(n, "sentinel") {
			t.Errorf("note quotes model text: %q", n)
		}
	}
}

// A root cause without a statement is no root cause: nothing coerced, nothing to log.
func TestNoStatementNoRootCauseAndNoNotes(t *testing.T) {
	got, notes := parseRootCause(t, map[string]any{"category": "bogus", "owner": "nobody"})
	if got != nil || notes != nil {
		t.Errorf("rootCause %v, notes %q", got, notes)
	}
}
