package node

import (
	"math/rand/v2"
	"testing"
	"time"
)

func TestBackoffRetriesFastForASecondThenDoublesToTheCap(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 1000; i++ {
		if d := Backoff(0, r); d < 0 || d >= 250*time.Millisecond {
			t.Fatalf("first retry %v", d)
		}
	}
	total := time.Duration(0)
	for n := 1; n <= 3; n++ {
		d := Backoff(n, r)
		if d != 250*time.Millisecond {
			t.Fatalf("attempt %d: %v", n, d)
		}
		total += d
	}
	if total > time.Second {
		t.Fatalf("the first second's retries take %v", total)
	}
	// Then 500 ms, 1, 2, 4 and 5 s, each 50-100% of that.
	for n, top := range map[int]time.Duration{4: 500 * time.Millisecond, 5: time.Second, 6: 2 * time.Second, 7: 4 * time.Second, 8: 5 * time.Second, 50: 5 * time.Second} {
		for i := 0; i < 200; i++ {
			d := Backoff(n, r)
			if d < top/2 || d > top {
				t.Fatalf("attempt %d: %v, want %v-%v", n, d, top/2, top)
			}
		}
	}
	if Backoff(1000, r) > BackoffCap {
		t.Fatal("past the cap")
	}
}
