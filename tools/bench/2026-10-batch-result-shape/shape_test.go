//go:build bench

// Compares the cost of three shapes a batch read could return, with half the
// keys missing as in a cache. See docs/research/2026-10-batch-result-shape.md.
//
//	go test -tags bench -bench . ./tools/bench/2026-10-batch-result-shape/
package bench

import (
	"errors"
	"fmt"
	"testing"
)

type result struct {
	Value string
	Err   error
}

var errNotFound = errors.New("key not found") // one shared instance: no allocation per miss

func keys(n int) []string {
	k := make([]string, n)
	for i := range k {
		k[i] = fmt.Sprintf("key:%06d", i)
	}
	return k
}

func BenchmarkShape(b *testing.B) {
	for _, n := range []int{100, 10000} {
		ks := keys(n)
		b.Run(fmt.Sprintf("map_values/n=%d", n), func(b *testing.B) { // map[string]T, misses absent
			b.ReportAllocs()
			for b.Loop() {
				m := make(map[string]string, n/2)
				for i, k := range ks {
					if i%2 == 0 {
						m[k] = "v"
					}
				}
			}
		})
		b.Run(fmt.Sprintf("map_results/n=%d", n), func(b *testing.B) { // map[string]Result, misses as errors
			b.ReportAllocs()
			for b.Loop() {
				m := make(map[string]result, n)
				for i, k := range ks {
					if i%2 == 0 {
						m[k] = result{Value: "v"}
					} else {
						m[k] = result{Err: errNotFound}
					}
				}
			}
		})
		b.Run(fmt.Sprintf("slice_results/n=%d", n), func(b *testing.B) { // []Result aligned with keys
			b.ReportAllocs()
			for b.Loop() {
				s := make([]result, n)
				for i := range ks {
					if i%2 == 0 {
						s[i] = result{Value: "v"}
					} else {
						s[i].Err = errNotFound
					}
				}
			}
		})
	}
}
