package store

import (
	"context"
	"testing"
)

// FTS5 reads bare AND/OR/NOT/NEAR as operators, so a query that has one of
// them as a word (or at either end, like "OR abstract") must still be treated
// as plain search text rather than failing as a syntax error.
func TestSearchItemsTreatsFTSOperatorsAsWords(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.WriteBatch(ctx, []ItemWrite{
		testWrite("abstract/or_not.jpg", nil),
		testWrite("abstract/other.jpg", nil),
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"OR abstract", "abstract OR", "AND", "NOT abstract", "NEAR abstract", `"abstract`, "abstract*"} {
		if _, err := s.SearchItems(ctx, q, "", 10); err != nil {
			t.Errorf("SearchItems(%q): %v", q, err)
		}
	}
	got, err := s.SearchItems(ctx, "OR abstract", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Relpath != "abstract/or_not.jpg" {
		t.Fatalf(`"OR abstract" = %v, want [abstract/or_not.jpg]`, relpaths(got))
	}
}
