package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/go-github/v88/github"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestBuildTimeline(t *testing.T) {
	now := ts("2024-01-05T12:00:00Z")
	tests := []struct {
		name      string
		items     []item
		wantStart time.Time
		wantDays  [][2]int
	}{
		{
			name:  "empty input",
			items: nil,
		},
		{
			name: "created and closed same day",
			items: []item{
				{Number: 1, Created: ts("2024-01-03T01:00:00Z"), Closed: ts("2024-01-03T23:00:00Z")},
			},
			wantStart: day("2024-01-03"),
			wantDays:  [][2]int{{1, 0}, {0, 0}, {0, 0}},
		},
		{
			name: "still open through today",
			items: []item{
				{Number: 1, Created: ts("2024-01-02T10:00:00Z")},
			},
			wantStart: day("2024-01-02"),
			wantDays:  [][2]int{{1, 0}, {1, 0}, {1, 0}, {1, 0}},
		},
		{
			name: "PRs and issues counted separately",
			items: []item{
				{Number: 1, Created: ts("2024-01-04T10:00:00Z")},
				{Number: 2, Created: ts("2024-01-04T11:00:00Z"), PR: true},
				{Number: 3, Created: ts("2024-01-05T11:00:00Z"), PR: true},
				{Number: 4, Created: ts("2024-01-04T12:00:00Z"), PR: true, Closed: ts("2024-01-04T13:00:00Z")},
			},
			wantStart: day("2024-01-04"),
			wantDays:  [][2]int{{1, 2}, {1, 2}},
		},
		{
			name: "closed today still counts today",
			items: []item{
				{Number: 1, Created: ts("2024-01-04T10:00:00Z"), Closed: ts("2024-01-05T01:00:00Z")},
			},
			wantStart: day("2024-01-04"),
			wantDays:  [][2]int{{1, 0}, {1, 0}},
		},
		{
			name: "oldest day is the minimum created date regardless of order",
			items: []item{
				{Number: 3, Created: ts("2024-01-04T10:00:00Z")},
				{Number: 1, Created: ts("2024-01-01T23:59:59Z"), Closed: ts("2024-01-02T00:00:00Z")},
				{Number: 2, Created: ts("2024-01-03T00:00:00Z"), PR: true, Closed: ts("2024-01-04T00:00:00Z")},
			},
			wantStart: day("2024-01-01"),
			wantDays:  [][2]int{{1, 0}, {1, 0}, {0, 1}, {1, 1}, {1, 0}},
		},
		{
			name: "non-UTC timestamps are bucketed by UTC date",
			items: []item{
				{Number: 1, Created: ts("2024-01-04T20:00:00-05:00")}, // 2024-01-05T01:00Z
			},
			wantStart: day("2024-01-05"),
			wantDays:  [][2]int{{1, 0}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildTimeline(tt.items, now)
			if !got.Start.Equal(tt.wantStart) {
				t.Errorf("start = %v, want %v", got.Start, tt.wantStart)
			}
			if len(got.Days) != len(tt.wantDays) || (len(got.Days) > 0 && !reflect.DeepEqual(got.Days, tt.wantDays)) {
				t.Errorf("days = %v, want %v", got.Days, tt.wantDays)
			}
		})
	}
}

func TestEncodeTimeline(t *testing.T) {
	tl := timeline{
		Start:   day("2014-03-28"),
		Updated: ts("2026-11-02T08:25:21Z"),
		Days:    [][2]int{{4, 0}, {4, 0}, {1777, 157}},
	}
	want := `{
  "start": "2014-03-28",
  "updated": "2026-11-02T08:25:21Z",
  "columns": ["open_issues", "open_prs"],
  "days": [
    [4, 0],
    [4, 0],
    [1777, 157]
  ]
}
`
	got := string(encodeTimeline(tl))
	if got != want {
		t.Fatalf("encodeTimeline =\n%s\nwant\n%s", got, want)
	}

	decoded, err := decodeTimeline([]byte(got))
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Start.Equal(tl.Start) || !decoded.Updated.Equal(tl.Updated) || !reflect.DeepEqual(decoded.Days, tl.Days) {
		t.Errorf("round trip = %+v, want %+v", decoded, tl)
	}
	if got := decoded.latestTotal(); got != 1934 {
		t.Errorf("latestTotal = %v, want 1934", got)
	}
}

func TestEncodeTimelineEmpty(t *testing.T) {
	enc := encodeTimeline(timeline{Start: day("2020-01-01"), Updated: ts("2020-01-01T00:00:00Z")})
	decoded, err := decodeTimeline(enc)
	if err != nil {
		t.Fatalf("decoding %s: %v", enc, err)
	}
	if len(decoded.Days) != 0 {
		t.Errorf("days = %v, want none", decoded.Days)
	}
}

func TestDecodeTimelineRejectsUnknownColumns(t *testing.T) {
	_, err := decodeTimeline([]byte(`{"start":"2020-01-01","columns":["a","b"],"days":[]}`))
	if err == nil {
		t.Fatal("expected an error for unexpected columns")
	}
}

func TestSetNextPage(t *testing.T) {
	tests := []struct {
		name     string
		resp     github.Response
		wantMore bool
		wantPage int
		wantTok  string
		wantAft  string
	}{
		{name: "last page", resp: github.Response{}, wantMore: false},
		{name: "offset", resp: github.Response{NextPage: 3}, wantMore: true, wantPage: 3},
		{name: "cursor with page", resp: github.Response{NextPage: 2, After: "abc"}, wantMore: true, wantPage: 2, wantAft: "abc"},
		{name: "cursor only", resp: github.Response{After: "abc"}, wantMore: true, wantAft: "abc"},
		{name: "string page token", resp: github.Response{NextPageToken: "tok"}, wantMore: true, wantTok: "tok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opt := &github.IssueListByRepoOptions{}
			opt.ListOptions.Page = 99
			opt.ListCursorOptions.After = "stale"
			more := setNextPage(opt, &tt.resp)
			if more != tt.wantMore {
				t.Fatalf("more = %v, want %v", more, tt.wantMore)
			}
			if !more {
				return
			}
			if opt.ListOptions.Page != tt.wantPage || opt.ListCursorOptions.Page != tt.wantTok || opt.ListCursorOptions.After != tt.wantAft {
				t.Errorf("got page=%v token=%q after=%q, want page=%v token=%q after=%q",
					opt.ListOptions.Page, opt.ListCursorOptions.Page, opt.ListCursorOptions.After,
					tt.wantPage, tt.wantTok, tt.wantAft)
			}
		})
	}
}
