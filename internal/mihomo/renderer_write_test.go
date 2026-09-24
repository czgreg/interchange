package mihomo

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

// TestWrite_ConcurrentWritersPublishCompleteRenders covers the scorer
// hot-reload goroutine and API re-renders calling Write at the same time.
// With the old shared "<path>.tmp" most writers failed with rename ENOENT and
// the published file was torn (matched no complete render) in 3 of 20 runs.
// Every writer must succeed and the final file must be byte-identical to one
// complete render.
func TestWrite_ConcurrentWritersPublishCompleteRenders(t *testing.T) {
	r := newTestRenderer(t)
	base := sampleOutbounds()

	const writers = 50
	renders := make([][]byte, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct content per writer so a cross-published file is
			// detectable, and varied size so partial writes would show.
			obs := make([]subscribe.Outbound, 0, len(base)*(1+i%5))
			for k := 0; k < 1+i%5; k++ {
				for _, o := range base {
					c := subscribe.Outbound{}
					for key, v := range o {
						c[key] = v
					}
					c["tag"] = fmt.Sprintf("w%02d-%d/%v", i, k, o["tag"])
					obs = append(obs, c)
				}
			}
			renders[i], errs[i] = r.Write(obs)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}
	final, err := os.ReadFile(r.Path())
	if err != nil {
		t.Fatalf("read final config: %v", err)
	}
	matched := false
	for _, b := range renders {
		if bytes.Equal(b, final) {
			matched = true
			break
		}
	}
	if !matched {
		t.Error("final config.yaml matches no writer's complete render")
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(r.Path()), "*.tmp"))
	if len(left) != 0 {
		t.Errorf("leftover temp files: %v", left)
	}
	if fi, err := os.Stat(r.Path()); err == nil && fi.Mode().Perm() != 0o644 {
		t.Errorf("config mode = %v, want 0644", fi.Mode().Perm())
	}
}
