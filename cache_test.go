package main

import (
	"errors"
	"maps"
	"strings"
	"testing"
	"time"
)

func TestChooseMode(t *testing.T) {
	now := ts("2026-11-02T01:15:00Z")
	fresh := &repoCache{FetchedAt: now.Add(-24 * time.Hour), LastFull: now.Add(-3 * 24 * time.Hour)}
	stale := &repoCache{FetchedAt: now.Add(-24 * time.Hour), LastFull: now.Add(-7*24*time.Hour - time.Minute)}
	tests := []struct {
		name      string
		prev      *repoCache
		forceFull bool
		want      fetchMode
	}{
		{name: "no cache file (e.g. newly added repo)", prev: nil, want: modeFull},
		{name: "no cache file with -full", prev: nil, forceFull: true, want: modeFull},
		{name: "last full older than 7 days", prev: stale, want: modeFull},
		{name: "last full exactly 7 days ago", prev: &repoCache{FetchedAt: now, LastFull: now.Add(-reconcileInterval)}, want: modeFull},
		{name: "-full flag overrides fresh cache", prev: fresh, forceFull: true, want: modeFull},
		{name: "fresh cache", prev: fresh, want: modeIncremental},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := chooseMode(tt.prev, tt.forceFull, now)
			if got != tt.want {
				t.Errorf("chooseMode = %v (%v), want %v", got, reason, tt.want)
			}
			if reason == "" {
				t.Error("empty reason")
			}
		})
	}
}

func TestUpsert(t *testing.T) {
	items := map[int]item{
		1: {Number: 1, Created: ts("2024-01-01T00:00:00Z")},
		2: {Number: 2, Created: ts("2024-01-02T00:00:00Z"), PR: true},
	}
	closed := item{Number: 1, Created: ts("2024-01-01T00:00:00Z"), Closed: ts("2024-01-03T00:00:00Z")}
	added := upsert(items, []item{
		closed,
		{Number: 3, Created: ts("2024-01-03T00:00:00Z")},
	})
	if added != 1 {
		t.Errorf("added = %v, want 1", added)
	}
	if len(items) != 3 {
		t.Errorf("len = %v, want 3", len(items))
	}
	if items[1] != closed {
		t.Errorf("item 1 = %+v, want %+v", items[1], closed)
	}
	if !items[2].PR {
		t.Error("untouched item 2 changed")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	c := &repoCache{
		FetchedAt: ts("2026-11-02T01:15:00Z"),
		LastFull:  ts("2026-10-30T01:15:00Z"),
		Items: map[int]item{
			7: {Number: 7, Created: ts("2024-01-01T10:00:00Z"), Closed: ts("2024-01-05T10:00:00Z"), PR: true},
			3: {Number: 3, Created: ts("2023-05-01T10:00:00Z")},
		},
	}
	enc, err := encodeCache(c)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"fetched_at":"2026-11-02T01:15:00Z","last_full":"2026-10-30T01:15:00Z","items":[{"n":3,"c":1682935200},{"n":7,"c":1704103200,"x":1704448800,"pr":true}]}`
	if string(enc) != want {
		t.Errorf("encodeCache =\n%s\nwant\n%s", enc, want)
	}
	got, err := decodeCache(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !got.FetchedAt.Equal(c.FetchedAt) || !got.LastFull.Equal(c.LastFull) || !maps.Equal(got.Items, c.Items) {
		t.Errorf("round trip = %+v, want %+v", got, c)
	}
}

func TestDecodeCacheRejectsMissingTimes(t *testing.T) {
	if _, err := decodeCache([]byte(`{"items":[]}`)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLoadCacheMissing(t *testing.T) {
	c, err := loadCache(t.TempDir() + "/nope.json")
	if c != nil || err != nil {
		t.Fatalf("loadCache = %v, %v; want nil, nil", c, err)
	}
}

func TestReconcile(t *testing.T) {
	mk := func(nums ...int) map[int]item {
		m := map[int]item{}
		for _, n := range nums {
			m[n] = item{Number: n, Created: ts("2024-01-01T00:00:00Z")}
		}
		return m
	}
	// #2 still exists (the listing dropped it) and was closed since; #3 is gone.
	lookup := func(n int) (item, bool, error) {
		switch n {
		case 2:
			return item{Number: 2, Created: ts("2024-01-01T00:00:00Z"), Closed: ts("2024-02-01T00:00:00Z")}, true, nil
		case 3:
			return item{}, false, nil
		}
		t.Fatalf("unexpected lookup of #%v", n)
		return item{}, false, nil
	}

	t.Run("recovers and drops", func(t *testing.T) {
		fresh := mk(1, 4)
		recovered, dropped, err := reconcile(mk(1, 2, 3), fresh, lookup, 10)
		if err != nil {
			t.Fatal(err)
		}
		if recovered != 1 || dropped != 1 {
			t.Errorf("recovered, dropped = %v, %v; want 1, 1", recovered, dropped)
		}
		if len(fresh) != 3 {
			t.Errorf("fresh has %v items, want 3 (1, 2, 4)", len(fresh))
		}
		if fresh[2].Closed.IsZero() {
			t.Error("recovered item should carry the looked-up data")
		}
		if _, ok := fresh[3]; ok {
			t.Error("gone item should not be kept")
		}
	})

	t.Run("nothing missing", func(t *testing.T) {
		recovered, dropped, err := reconcile(mk(1), mk(1, 4), lookup, 10)
		if err != nil || recovered != 0 || dropped != 0 {
			t.Errorf("got %v, %v, %v", recovered, dropped, err)
		}
	})

	t.Run("too many missing", func(t *testing.T) {
		fresh := mk(1)
		_, _, err := reconcile(mk(1, 2, 3), fresh, lookup, 1)
		if err == nil || !strings.Contains(err.Error(), "incomplete") {
			t.Errorf("err = %v, want an incomplete-listing error", err)
		}
		if len(fresh) != 1 {
			t.Error("fresh should be untouched on error")
		}
	})

	t.Run("lookup error", func(t *testing.T) {
		boom := errors.New("boom")
		_, _, err := reconcile(mk(1, 2), mk(1), func(int) (item, bool, error) { return item{}, false, boom }, 10)
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want %v", err, boom)
		}
	})
}

func TestCheckShrink(t *testing.T) {
	old := timeline{Days: [][2]int{{0, 0}, {80, 20}}}
	tests := []struct {
		name    string
		latest  [2]int
		wantErr bool
	}{
		{name: "unchanged", latest: [2]int{80, 20}},
		{name: "grew", latest: [2]int{90, 30}},
		{name: "exactly 90%", latest: [2]int{70, 20}},
		{name: "below 90%", latest: [2]int{70, 19}, wantErr: true},
		{name: "empty", latest: [2]int{0, 0}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkShrink(old, timeline{Days: [][2]int{tt.latest}})
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
