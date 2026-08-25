package obs

import (
	"sync"
	"testing"
)

func TestConcurrentInc(t *testing.T) {
	c := NewCounterMap()
	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				c.Inc("hits", 1)
			}
		}()
	}
	wg.Wait()
	if got := c.Get("hits"); got != 10000 {
		t.Fatalf("hits = %d, want 10000", got)
	}
}

func TestMetricsTextSortedStable(t *testing.T) {
	c := NewCounterMap()
	c.Add("z", 1)
	c.Add("a", 2)
	out := c.MetricsText()
	want := "a 2\nz 1\n"
	if out != want {
		t.Fatalf("metrics text = %q, want %q", out, want)
	}
}

func TestHealthNow(t *testing.T) {
	h := HealthNow(true)
	if h.Status != "ok" || h.CheckedAt <= 0 {
		t.Fatalf("health = %+v", h)
	}
}
