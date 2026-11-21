package handlers

import "testing"

// R-8: list paging must clamp hard — the inventory and customers endpoints
// turned their silent caps (500/200/20) into real pages, and a hostile or
// buggy limit value must never widen the query past the configured maximum.
func TestParseListPagingRaw(t *testing.T) {
        const (
                def = 500
                max = 1000
        )

        cases := []struct {
                name           string
                limitRaw       string
                offsetRaw      string
                wantLimit      int
                wantOffset     int
        }{
                {"no params -> defaults", "", "", def, 0},
                {"plain values pass through", "50", "120", 50, 120},
                {"limit below 1 falls back to default", "0", "", def, 0},
                {"negative limit falls back to default", "-5", "", def, 0},
                {"negative offset falls back to zero", "", "-3", def, 0},
                {"non-numeric falls back", "abc", "xyz", def, 0},
                {"limit above max clamps to max", "99999", "", max, 0},
                {"limit at max stays", "1000", "", 1000, 0},
                {"whitespace trimmed", " 25 ", " 7 ", 25, 7},
                {"float rejected -> default", "12.5", "", def, 0},
        }

        for _, tc := range cases {
                t.Run(tc.name, func(t *testing.T) {
                        gotLimit, gotOffset := parseListPagingRaw(tc.limitRaw, tc.offsetRaw, def, max)
                        if gotLimit != tc.wantLimit || gotOffset != tc.wantOffset {
                                t.Fatalf("parseListPagingRaw(%q,%q) = (%d,%d), want (%d,%d)",
                                        tc.limitRaw, tc.offsetRaw, gotLimit, gotOffset, tc.wantLimit, tc.wantOffset)
                        }
                })
        }
}
