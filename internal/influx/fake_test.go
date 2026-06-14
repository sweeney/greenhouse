package influx

import (
	"context"
	"testing"
)

// TestFakeQuerier_DeterministicLongestMatch pins the documented contract: when
// more than one programmed key is a substring of the flux, resolution is
// DETERMINISTIC — the longest matching key wins. Before the fix Query iterated
// the Responses map directly, and Go randomizes map iteration order, so which of
// two overlapping keys won was a coin flip per run (a latent flaky test).
//
// "abc" contains both "ab" and "abc" as substrings, so both keys match; the
// longest ("abc") must win on every iteration.
func TestFakeQuerier_DeterministicLongestMatch(t *testing.T) {
	short := []Row{{DeviceID: "short", Value: 1, HasValue: true}}
	long := []Row{{DeviceID: "long", Value: 2, HasValue: true}}

	// Run many times: with non-deterministic map iteration this loop would, with
	// overwhelming probability, observe the short key win at least once.
	for i := 0; i < 500; i++ {
		f := &FakeQuerier{Responses: map[string][]Row{
			"ab":  short,
			"abc": long,
		}}
		rows, err := f.Query(context.Background(), "value abc here")
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(rows) != 1 || rows[0].DeviceID != "long" {
			t.Fatalf("iteration %d: longest key did not win; got %+v", i, rows)
		}
	}
}

// TestFakeQuerier_LongestTieBreak checks that equal-length keys break ties
// lexicographically (stable), and that a non-overlapping match still resolves.
func TestFakeQuerier_LongestTieBreak(t *testing.T) {
	a := []Row{{DeviceID: "a"}}
	b := []Row{{DeviceID: "b"}}
	for i := 0; i < 200; i++ {
		f := &FakeQuerier{Responses: map[string][]Row{
			"xx": a,
			"yy": b,
		}}
		// Flux contains both equal-length keys; "xx" sorts before "yy".
		rows, err := f.Query(context.Background(), "yy xx")
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(rows) != 1 || rows[0].DeviceID != "a" {
			t.Fatalf("iteration %d: tie-break not lexicographic; got %+v", i, rows)
		}
	}
}
