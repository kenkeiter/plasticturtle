package shell

import (
	"context"
	"strings"
	"testing"
)

// stripEscapes removes SGR/erase sequences so a rendered row can be measured
// as the terminal would print it.
func stripEscapes(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			continue
		}
		if i+1 >= len(s) || s[i+1] != '[' {
			t.Fatalf("render emitted a non-CSI escape in %q", s)
		}
		i += 2
		for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
			i++
		}
	}
	return b.String()
}

func renderedStats(image string, warn bool, st bannerStats) *banner {
	b := newBanner(image, warn, "pt-test-instance", 0, false)
	b.stats = st
	return b
}

func TestBannerRenderWideHasBothSides(t *testing.T) {
	b := renderedStats("pt-proj-abc123", true, bannerStats{shells: 2, cpuPct: 87.4, memGB: 2.35, haveUsage: true})
	out := b.render(120)
	plain := stripEscapes(t, out)

	for _, want := range []string{
		"🐢 Sandbox", "[pt-proj-abc123]", "⚠️ unrestricted network",
		"2 shells → 87% CPU / 2.4GB MEM",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("render missing %q in %q", want, plain)
		}
	}
	if w := cellWidth(plain); w > 120 {
		t.Errorf("row is %d cells, wider than the 120-cell terminal", w)
	}
}

func TestBannerRenderNeverOverflows(t *testing.T) {
	// Overflow at the bottom row wraps the cursor into the guest's rows, so
	// this property must hold at every width, not just plausible ones.
	b := renderedStats("pt-some-long-project-name-here", true,
		bannerStats{shells: 12, cpuPct: 1234.5, memGB: 61.25, haveUsage: true})
	for width := 1; width <= 200; width++ {
		plain := stripEscapes(t, b.render(width))
		if w := cellWidth(plain); w > width {
			t.Fatalf("width %d: row is %d cells", width, w)
		}
	}
}

func TestBannerRenderNarrowDropsRightSide(t *testing.T) {
	b := renderedStats("pt-proj-abc123", true, bannerStats{shells: 2, cpuPct: 50, memGB: 1, haveUsage: true})
	plain := stripEscapes(t, b.render(60))
	if strings.Contains(plain, "CPU") {
		t.Errorf("right side survived a 60-cell terminal: %q", plain)
	}
	if !strings.Contains(plain, "unrestricted network") {
		t.Errorf("left side lost the warning before the figures: %q", plain)
	}
}

func TestBannerRenderTinyKeepsTheTurtle(t *testing.T) {
	b := renderedStats("pt-proj-abc123", true, bannerStats{shells: 1})
	plain := stripEscapes(t, b.render(8))
	if !strings.Contains(plain, "🐢") {
		t.Errorf("even the turtle drowned: %q", plain)
	}
}

func TestBannerRenderRestrictedHasNoWarning(t *testing.T) {
	b := renderedStats("pt-proj-abc123", false, bannerStats{shells: 1})
	plain := stripEscapes(t, b.render(120))
	if strings.Contains(plain, "unrestricted") {
		t.Errorf("warning shown for a restricted network: %q", plain)
	}
}

func TestBannerRenderTruncatesLongImageKeepingTail(t *testing.T) {
	// An OCI reference identifies itself at the end; the registry prefix is
	// the disposable part.
	b := renderedStats("ghcr.io/cirruslabs/macos-tahoe-base:latest", true, bannerStats{shells: 1})
	plain := stripEscapes(t, b.render(160))
	if !strings.Contains(plain, "macos-tahoe-base:latest]") {
		t.Errorf("image tail not kept: %q", plain)
	}
	if strings.Contains(plain, "ghcr.io") {
		t.Errorf("image not truncated: %q", plain)
	}
	if !strings.Contains(plain, "[…") {
		t.Errorf("truncation not marked at the front: %q", plain)
	}
}

func TestTruncateTail(t *testing.T) {
	for _, c := range []struct {
		s    string
		max  int
		want string
	}{
		{"tahoe-base", 24, "tahoe-base"},
		{"ghcr.io/cirruslabs/macos-tahoe-base:latest", 24, "…macos-tahoe-base:latest"},
		{"abcdef", 3, "…ef"},
		{"abcdef", 1, "…"},
	} {
		if got := truncateTail(c.s, c.max); got != c.want {
			t.Errorf("truncateTail(%q, %d) = %q, want %q", c.s, c.max, got, c.want)
		}
		if w := cellWidth(truncateTail(c.s, c.max)); w > c.max {
			t.Errorf("truncateTail(%q, %d) is %d cells wide", c.s, c.max, w)
		}
	}
}

func TestBannerRightTextForms(t *testing.T) {
	b := newBanner("img", false, "pt-test-instance", 0, false)
	cases := []struct {
		st   bannerStats
		want string
	}{
		{bannerStats{shells: 1, cpuPct: 12.6, memGB: 0.51, haveUsage: true}, "1 shell → 13% CPU / 0.5GB MEM "},
		{bannerStats{shells: 3, cpuPct: 250, memGB: 8, haveUsage: true}, "3 shells → 250% CPU / 8.0GB MEM "},
		{bannerStats{shells: 2}, "2 shells "},
		{bannerStats{}, ""},
	}
	for _, c := range cases {
		if got := b.rightText(c.st); got != c.want {
			t.Errorf("rightText(%+v) = %q, want %q", c.st, got, c.want)
		}
	}
}

// fakeUsageWorld substitutes the lsof and ps seams for one test, returning a
// call log so caching behaviour is observable.
func fakeUsageWorld(t *testing.T, lsofPIDs []int, ps map[int][2]float64) (lsofCalls, psCalls *int) {
	t.Helper()
	lsofCalls, psCalls = new(int), new(int)

	origLsof, origPs := lsofOpenPIDs, psUsage
	t.Cleanup(func() { lsofOpenPIDs, psUsage = origLsof, origPs })

	lsofOpenPIDs = func(ctx context.Context, path string) []int {
		*lsofCalls++
		return lsofPIDs
	}
	psUsage = func(ctx context.Context, pids []int) (float64, float64, int, error) {
		*psCalls++
		var cpu, rss float64
		rows := 0
		for _, pid := range pids {
			if v, ok := ps[pid]; ok {
				cpu += v[0]
				rss += v[1]
				rows++
			}
		}
		return cpu, rss, rows, nil
	}
	return lsofCalls, psCalls
}

func TestUsageSamplerSumsHelperAndTart(t *testing.T) {
	// The Virtualization helper (231%, 10.5GB) is where the VM lives; tart
	// itself idles at 45MB. The banner must report their sum, not tart alone —
	// sampling only tart is the bug that shipped first.
	lsofCalls, _ := fakeUsageWorld(t,
		[]int{901, 790}, // helper + tart, as lsof reports them
		map[int][2]float64{901: {231.0, 10.5 * 1024 * 1024}, 790: {0.0, 45 * 1024}},
	)

	u := newUsageSampler("pt-test-vm", 790)
	cpu, memGB, err := u.sample(context.Background())
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if cpu != 231.0 {
		t.Errorf("cpu = %v, want 231.0", cpu)
	}
	if memGB < 10.5 || memGB > 10.6 {
		t.Errorf("memGB = %v, want ~10.5", memGB)
	}
	if *lsofCalls != 1 {
		t.Errorf("lsof ran %d times for one sample, want 1", *lsofCalls)
	}
}

func TestUsageSamplerCachesResolution(t *testing.T) {
	lsofCalls, psCalls := fakeUsageWorld(t,
		[]int{901},
		map[int][2]float64{901: {50, 1024 * 1024}},
	)

	u := newUsageSampler("pt-test-vm", 790)
	for range 5 {
		if _, _, err := u.sample(context.Background()); err != nil {
			t.Fatalf("sample: %v", err)
		}
	}
	if *lsofCalls != 1 {
		t.Errorf("lsof ran %d times across 5 samples, want 1 (it costs ~200ms)", *lsofCalls)
	}
	if *psCalls != 5 {
		t.Errorf("ps ran %d times, want 5", *psCalls)
	}
}

func TestUsageSamplerFallsBackToTartPID(t *testing.T) {
	// No helper holds the disk open (older macOS runs the VM inside tart):
	// the recorded tart PID is the whole answer.
	fakeUsageWorld(t, nil, map[int][2]float64{790: {12, 2 * 1024 * 1024}})

	u := newUsageSampler("pt-test-vm", 790)
	cpu, memGB, err := u.sample(context.Background())
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if cpu != 12 || memGB != 2 {
		t.Errorf("got %v%% / %vGB, want 12%% / 2GB", cpu, memGB)
	}
}

func TestUsageSamplerReresolvesWhenProcessesVanish(t *testing.T) {
	lsofCalls, _ := fakeUsageWorld(t, []int{901}, nil) // ps knows nobody

	u := newUsageSampler("pt-test-vm", 0)
	if _, _, err := u.sample(context.Background()); err == nil {
		t.Fatal("sample of vanished processes reported success")
	}
	if _, _, err := u.sample(context.Background()); err == nil {
		t.Fatal("second sample reported success")
	}
	if *lsofCalls != 2 {
		t.Errorf("lsof ran %d times, want 2: a dead set must be re-resolved, not re-polled", *lsofCalls)
	}
}

func TestUsageSamplerErrorsWithNothingToSample(t *testing.T) {
	fakeUsageWorld(t, nil, nil)
	u := newUsageSampler("pt-test-vm", 0)
	if _, _, err := u.sample(context.Background()); err == nil {
		t.Fatal("sample with no resolvable process reported success")
	}
}

func TestCellWidthOverestimatesAmbiguousRunes(t *testing.T) {
	// ⚠ and → are East-Asian-ambiguous: some terminals draw them one cell
	// wide, some two. The banner must budget two — undercounting wraps the
	// bottom row.
	for _, c := range []struct {
		s    string
		want int
	}{
		{"abc", 3},
		{"→", 2},
		{"⚠️", 2}, // U+26A0 counted 2; the variation selector counts 0
		{"🐢", 2},
	} {
		if got := cellWidth(c.s); got != c.want {
			t.Errorf("cellWidth(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}
