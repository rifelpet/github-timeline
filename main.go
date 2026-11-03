// Command github-timeline downloads the issues and pull requests of a GitHub
// repo and writes a per-day count of open issues and open PRs to
// data/{org}/{repo}.json.
//
// Usage:
//
//	go run main.go [-full] org/repo
//
// Everything lives in this one file so that `go run main.go` keeps working.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofri/go-github-ratelimit/v2/github_ratelimit"
	"github.com/google/go-github/v93/github"
)

const (
	// perPage is the GitHub API maximum page size for the issues endpoint.
	// Larger pages mean fewer requests, which is both faster and less likely
	// to hit a transient failure mid-pagination.
	perPage = 100

	// maxRetries bounds how many times a single request is retried before
	// giving up. GitHub occasionally returns transient 401/403/5xx responses
	// on long-running paginated jobs; retrying avoids losing the whole run.
	maxRetries = 6

	// fetchTimeout caps how long fetching a single repo's issues may take, so
	// a stuck connection can't hang the job indefinitely.
	fetchTimeout = 50 * time.Minute
)

// retryBackoff is the delay before the first retry; it doubles after each.
// A variable so tests can shorten it.
var retryBackoff = time.Second

func main() {
	full := flag.Bool("full", false, "download every issue and PR and reconcile the cache against it, instead of fetching only what changed since the last run")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %v [-full] org/repo\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	org, repo, ok := strings.Cut(flag.Arg(0), "/")
	if !ok || org == "" || repo == "" || strings.Contains(repo, "/") {
		log.Fatalf("expected the repo as org/repo, got %q", flag.Arg(0))
	}
	if err := run(org, repo, *full); err != nil {
		log.Fatalf("%v/%v: %v", org, repo, err)
	}
}

func run(org, repo string, forceFull bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	// Let go-github sleep through (rather than error on) primary rate limits.
	ctx = context.WithValue(ctx, github.SleepUntilPrimaryRateLimitResetWhenRateLimited, true)

	dataPath := filepath.Join("data", org, repo+".json")
	cachePath := filepath.Join("cache", org, repo+".json")

	startedAt := time.Now().UTC().Truncate(time.Second)
	prev, err := loadCache(cachePath)
	if err != nil {
		log.Printf("%v/%v: ignoring unreadable cache %v: %v", org, repo, cachePath, err)
		prev = nil
	}
	mode, reason := chooseMode(prev, forceFull, startedAt)
	log.Printf("%v/%v: %v fetch (%v)", org, repo, mode, reason)

	client, err := newClient()
	if err != nil {
		return err
	}

	next := &repoCache{FetchedAt: startedAt}
	switch mode {
	case modeIncremental:
		since := prev.FetchedAt.Add(-sinceOverlap)
		issues, err := listIssues(ctx, client, org, repo, github.IssueListByRepoOptions{
			State:     "all",
			Sort:      "updated",
			Direction: "asc",
			Since:     since,
		})
		if err != nil {
			return err
		}
		next.LastFull = prev.LastFull
		next.Items = prev.Items
		added := upsert(next.Items, itemsFromIssues(issues))
		log.Printf("%v/%v: %v items updated since %v (%v new), %v cached in total",
			org, repo, len(issues), since.Format(time.RFC3339), added, len(next.Items))

	case modeFull:
		issues, err := listIssues(ctx, client, org, repo, github.IssueListByRepoOptions{State: "all"})
		if err != nil {
			return err
		}
		next.LastFull = startedAt
		next.Items = make(map[int]item, len(issues))
		upsert(next.Items, itemsFromIssues(issues))
		if prev != nil {
			repoURL := fmt.Sprintf("%vrepos/%v/%v", client.BaseURL(), org, repo)
			if len(issues) > 0 && issues[0].GetRepositoryURL() != "" {
				repoURL = issues[0].GetRepositoryURL()
			}
			lookup := func(n int) (item, bool, error) {
				return lookupIssue(ctx, client, org, repo, repoURL, n)
			}
			recovered, dropped, err := reconcile(prev.Items, next.Items, lookup, maxLookups)
			if err != nil {
				return err
			}
			log.Printf("%v/%v: full listing returned %v items; of the cached items it lacked, %v were recovered by lookup and %v dropped as gone",
				org, repo, len(issues), recovered, dropped)
		}
	}

	if len(next.Items) == 0 {
		return fmt.Errorf("no issues returned; refusing to overwrite data with an empty timeline")
	}
	finishedAt := time.Now().UTC()
	tl := buildTimeline(slices.Collect(maps.Values(next.Items)), finishedAt)
	tl.Updated = finishedAt

	if prev == nil {
		if err := checkExistingData(dataPath, tl); err != nil {
			return err
		}
	}
	if err := writeFileAtomic(dataPath, encodeTimeline(tl)); err != nil {
		return err
	}
	enc, err := encodeCache(next)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(cachePath, enc); err != nil {
		return err
	}
	last := tl.Days[len(tl.Days)-1]
	log.Printf("%v/%v: wrote %v days to %v; today: %v open issues, %v open PRs", org, repo, len(tl.Days), dataPath, last[0], last[1])
	return nil
}

func itemsFromIssues(issues []*github.Issue) []item {
	items := make([]item, len(issues))
	for i, issue := range issues {
		items[i] = itemFromIssue(issue)
	}
	return items
}

// checkExistingData applies checkShrink against the data file at path, if
// there is a readable one.
func checkExistingData(path string, tl timeline) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	old, err := decodeTimeline(data)
	if err != nil {
		log.Printf("skipping size check against unreadable %v: %v", path, err)
		return nil
	}
	return checkShrink(old, tl)
}

// writeFileAtomic writes data to a temporary file next to path and renames it
// into place, so an interrupted run never leaves a truncated file behind.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op once renamed
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// newClient returns a GitHub client that waits out secondary rate limits and
// authenticates with $GITHUB_TOKEN when set.
func newClient() (*github.Client, error) {
	rateLimiter, err := github_ratelimit.NewRateLimitWaiterClient(nil)
	if err != nil {
		return nil, fmt.Errorf("creating rate limiter client: %w", err)
	}
	opts := []github.ClientOptionsFunc{github.WithHTTPClient(rateLimiter)}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		opts = append(opts, github.WithAuthToken(token))
	}
	return github.NewClient(opts...)
}

// withRetry runs a single API call, retrying transient failures (network
// errors and 401/403/429/5xx responses) with exponential backoff. GitHub
// intermittently returns these on long paginated jobs, and without retries a
// single blip aborts the entire scrape.
func withRetry[T any](ctx context.Context, desc string, call func() (T, *github.Response, error)) (T, *github.Response, error) {
	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			backoff := retryBackoff << (attempt - 1)
			log.Printf("retrying %v (attempt %v/%v) after %v: %v", desc, attempt+1, maxRetries, backoff, lastErr)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				var zero T
				return zero, nil, ctx.Err()
			}
		}
		v, resp, err := call()
		if err == nil {
			return v, resp, nil
		}
		if !isRetryable(err) {
			return v, resp, err
		}
		lastErr = err
	}
	var zero T
	return zero, nil, fmt.Errorf("exhausted %v retries: %w", maxRetries, lastErr)
}

// isRetryable reports whether err is worth retrying. Context cancellation and
// non-transient HTTP statuses (e.g. 404) are not; transient HTTP statuses and
// transport-level errors are.
func isRetryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var rateErr *github.RateLimitError
	if errors.As(err, &rateErr) {
		return true
	}
	var abuseErr *github.AbuseRateLimitError
	if errors.As(err, &abuseErr) {
		return true
	}
	if code := statusCode(err); code != 0 {
		switch code {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
			return true
		}
		return code >= 500
	}
	// No structured HTTP response: most likely a transport/network error. Retry.
	return true
}

// statusCode returns the HTTP status of a GitHub API error, or 0 if err did
// not come with a response.
func statusCode(err error) int {
	var errResp *github.ErrorResponse
	if errors.As(err, &errResp) && errResp.Response != nil {
		return errResp.Response.StatusCode
	}
	return 0
}

// setNextPage points opt at the page following resp, mirroring whatever the
// Link: rel="next" header carried. The issues endpoint switches to cursor
// pagination (an "after" cursor alongside "page") for deep result sets, so
// the cursor must be forwarded too; offset paging alone fails past a certain
// depth. It returns false when there is no next page.
func setNextPage(opt *github.IssueListByRepoOptions, resp *github.Response) bool {
	if resp.NextPage == 0 && resp.NextPageToken == "" && resp.After == "" && resp.Cursor == "" {
		return false
	}
	opt.ListOptions.Page = resp.NextPage
	opt.ListCursorOptions.Page = ""
	if resp.NextPage == 0 {
		opt.ListCursorOptions.Page = resp.NextPageToken
	}
	opt.ListCursorOptions.After = resp.After
	opt.ListCursorOptions.Cursor = resp.Cursor
	return true
}

// listIssues fetches every issue and PR matching opt, one page at a time, so
// a transient failure only retries the page it happened on.
func listIssues(ctx context.Context, client *github.Client, org, repo string, opt github.IssueListByRepoOptions) ([]*github.Issue, error) {
	opt.ListOptions.PerPage = perPage
	var all []*github.Issue
	for page := 1; ; page++ {
		desc := fmt.Sprintf("page %v of %v/%v", page, org, repo)
		issues, resp, err := withRetry(ctx, desc, func() ([]*github.Issue, *github.Response, error) {
			return client.Issues.ListByRepo(ctx, org, repo, &opt)
		})
		if err != nil {
			return nil, fmt.Errorf("fetching %v: %w", desc, err)
		}
		all = append(all, issues...)
		more := setNextPage(&opt, resp)
		if page%10 == 0 || !more {
			log.Printf("fetched %v pages, %v items for %v/%v", page, len(all), org, repo)
		}
		if !more {
			return all, nil
		}
	}
}

// itemFromIssue extracts the fields the timeline needs.
func itemFromIssue(issue *github.Issue) item {
	it := item{
		Number:  issue.GetNumber(),
		Created: issue.GetCreatedAt().UTC(),
		PR:      issue.IsPullRequest(),
	}
	if issue.ClosedAt != nil {
		it.Closed = issue.GetClosedAt().UTC()
	}
	return it
}

// lookupIssue fetches a single issue or PR, reporting found=false if it no
// longer belongs to the repo whose canonical API URL is repoURL (as listed in
// repository_url). Deleted and hidden items return 404 or 410; transferred
// ones redirect to their new repo, which shows in repository_url.
func lookupIssue(ctx context.Context, client *github.Client, org, repo, repoURL string, number int) (item, bool, error) {
	desc := fmt.Sprintf("lookup of %v/%v#%v", org, repo, number)
	issue, _, err := withRetry(ctx, desc, func() (*github.Issue, *github.Response, error) {
		return client.Issues.Get(ctx, org, repo, number)
	})
	if err != nil {
		switch statusCode(err) {
		case http.StatusNotFound, http.StatusGone, http.StatusUnavailableForLegalReasons:
			return item{}, false, nil
		}
		return item{}, false, err
	}
	if got := issue.GetRepositoryURL(); !strings.EqualFold(got, repoURL) {
		log.Printf("%v: moved to %v", desc, got)
		return item{}, false, nil
	}
	return itemFromIssue(issue), true, nil
}

// item is the minimal view of an issue or pull request the timeline needs.
type item struct {
	Number  int
	Created time.Time
	// Closed is the zero time while the item is open. GitHub only reports the
	// most recent close, so an item closed, reopened and closed again counts
	// as open for the whole span between its creation and its last close.
	Closed time.Time
	PR     bool
}

// columns names the values in each row of timeline.Days, in order.
var columns = []string{"open_issues", "open_prs"}

// timeline is the per-day count of open issues and PRs written to
// data/{org}/{repo}.json. Days[i] is the UTC date Start + i days; the last row
// is the (partial) day the timeline was built.
type timeline struct {
	Start   time.Time
	Updated time.Time
	Days    [][2]int
}

// dayOf truncates t to midnight UTC of the same UTC date.
func dayOf(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// daysBetween returns the number of whole days from day a to day b, both of
// which must be UTC midnights.
func daysBetween(a, b time.Time) int {
	return int(b.Sub(a).Hours() / 24)
}

// buildTimeline counts, for every day from the oldest item's creation through
// now, how many issues and PRs were open at some point that day. An item is
// counted on both its creation day and its closing day. Items may be given in
// any order.
func buildTimeline(items []item, now time.Time) timeline {
	if len(items) == 0 {
		return timeline{}
	}
	start := dayOf(items[0].Created)
	for _, it := range items[1:] {
		if d := dayOf(it.Created); d.Before(start) {
			start = d
		}
	}
	numDays := daysBetween(start, dayOf(now)) + 1
	if numDays < 1 {
		numDays = 1
	}

	// Difference array: +1 on the first open day, -1 the day after the last.
	diff := make([][2]int, numDays+1)
	for _, it := range items {
		first := daysBetween(start, dayOf(it.Created))
		last := numDays - 1
		if !it.Closed.IsZero() {
			last = daysBetween(start, dayOf(it.Closed))
		}
		last = min(last, numDays-1)
		if first > last {
			continue
		}
		col := 0
		if it.PR {
			col = 1
		}
		diff[first][col]++
		diff[last+1][col]--
	}

	days := make([][2]int, numDays)
	var running [2]int
	for i := range days {
		running[0] += diff[i][0]
		running[1] += diff[i][1]
		days[i] = running
	}
	return timeline{Start: start, Days: days}
}

// encodeTimeline renders t as JSON with one day per line, so daily data
// commits produce small, legible diffs.
func encodeTimeline(t timeline) []byte {
	var b bytes.Buffer
	b.WriteString("{\n")
	fmt.Fprintf(&b, "  \"start\": %q,\n", t.Start.UTC().Format(time.DateOnly))
	fmt.Fprintf(&b, "  \"updated\": %q,\n", t.Updated.UTC().Format(time.RFC3339))
	cols, _ := json.Marshal(columns)
	fmt.Fprintf(&b, "  \"columns\": %s,\n", bytes.ReplaceAll(cols, []byte(","), []byte(", ")))
	if len(t.Days) == 0 {
		b.WriteString("  \"days\": []\n")
	} else {
		b.WriteString("  \"days\": [\n")
		for i, d := range t.Days {
			b.WriteString("    [")
			b.WriteString(strconv.Itoa(d[0]))
			b.WriteString(", ")
			b.WriteString(strconv.Itoa(d[1]))
			b.WriteString("]")
			if i < len(t.Days)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString("  ]\n")
	}
	b.WriteString("}\n")
	return b.Bytes()
}

// decodeTimeline parses a data file written by encodeTimeline.
func decodeTimeline(data []byte) (timeline, error) {
	var raw struct {
		Start   string    `json:"start"`
		Updated time.Time `json:"updated"`
		Columns []string  `json:"columns"`
		Days    [][2]int  `json:"days"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return timeline{}, err
	}
	if !slices.Equal(raw.Columns, columns) {
		return timeline{}, fmt.Errorf("unexpected columns %q, want %q", raw.Columns, columns)
	}
	start, err := time.Parse(time.DateOnly, raw.Start)
	if err != nil {
		return timeline{}, fmt.Errorf("invalid start: %w", err)
	}
	return timeline{Start: start, Updated: raw.Updated.UTC(), Days: raw.Days}, nil
}

// latestTotal returns open issues + open PRs on the last day of t.
func (t timeline) latestTotal() int {
	if len(t.Days) == 0 {
		return 0
	}
	last := t.Days[len(t.Days)-1]
	return last[0] + last[1]
}

const (
	// reconcileInterval is how long incremental fetches are trusted before a
	// full download. Incremental fetches (since=) only see items whose
	// updated_at changed, so they miss items that were deleted, transferred
	// out, converted to discussions or hidden as spam; the periodic full
	// fetch drops those.
	reconcileInterval = 7 * 24 * time.Hour

	// sinceOverlap is subtracted from the previous fetch's start time to
	// cover clock skew and items whose updated_at lands mid-crawl.
	sinceOverlap = time.Hour

	// maxLookups caps how many cached items missing from a full download are
	// looked up individually. More than this means the full listing itself
	// is broken, and nothing is written.
	maxLookups = 500
)

// cacheFile is the on-disk per-repo cache at cache/{org}/{repo}.json. It is
// not committed; the workflow persists it with actions/cache.
type cacheFile struct {
	// FetchedAt is when the last successful fetch started.
	FetchedAt time.Time `json:"fetched_at"`
	// LastFull is when the last successful full fetch started.
	LastFull time.Time   `json:"last_full"`
	Items    []cacheItem `json:"items"`
}

// cacheItem is an item with short keys and unix-second timestamps to keep
// the cache small (kubernetes/kubernetes has well over 100k items).
type cacheItem struct {
	N  int   `json:"n"`
	C  int64 `json:"c"`
	X  int64 `json:"x,omitempty"` // closed; omitted while open
	PR bool  `json:"pr,omitempty"`
}

// repoCache is the in-memory form of cacheFile, keyed by issue number.
type repoCache struct {
	FetchedAt time.Time
	LastFull  time.Time
	Items     map[int]item
}

// loadCache reads the cache at path. It returns (nil, nil) if there is none.
func loadCache(path string) (*repoCache, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeCache(data)
}

func decodeCache(data []byte) (*repoCache, error) {
	var f cacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.FetchedAt.IsZero() || f.LastFull.IsZero() {
		return nil, fmt.Errorf("cache is missing fetched_at or last_full")
	}
	c := &repoCache{FetchedAt: f.FetchedAt.UTC(), LastFull: f.LastFull.UTC(), Items: make(map[int]item, len(f.Items))}
	for _, ci := range f.Items {
		it := item{Number: ci.N, Created: time.Unix(ci.C, 0).UTC(), PR: ci.PR}
		if ci.X != 0 {
			it.Closed = time.Unix(ci.X, 0).UTC()
		}
		c.Items[ci.N] = it
	}
	return c, nil
}

// encodeCache serializes c compactly, ordered by issue number.
func encodeCache(c *repoCache) ([]byte, error) {
	f := cacheFile{FetchedAt: c.FetchedAt.UTC(), LastFull: c.LastFull.UTC(), Items: make([]cacheItem, 0, len(c.Items))}
	for _, n := range slices.Sorted(maps.Keys(c.Items)) {
		it := c.Items[n]
		ci := cacheItem{N: n, C: it.Created.Unix(), PR: it.PR}
		if !it.Closed.IsZero() {
			ci.X = it.Closed.Unix()
		}
		f.Items = append(f.Items, ci)
	}
	return json.Marshal(f)
}

// upsert replaces or adds each of fresh in items, keyed by number. It
// returns how many were new.
func upsert(items map[int]item, fresh []item) (added int) {
	for _, it := range fresh {
		if _, ok := items[it.Number]; !ok {
			added++
		}
		items[it.Number] = it
	}
	return added
}

// fetchMode is how a repo's items are fetched on a given run.
type fetchMode int

const (
	modeFull fetchMode = iota
	modeIncremental
)

func (m fetchMode) String() string {
	if m == modeIncremental {
		return "incremental"
	}
	return "full"
}

// chooseMode decides between a full and an incremental fetch for one repo,
// based only on that repo's own cache (prev is nil when it has none, e.g. a
// repo newly added to repos.json, which therefore gets its full history).
func chooseMode(prev *repoCache, forceFull bool, now time.Time) (fetchMode, string) {
	switch {
	case forceFull:
		return modeFull, "-full flag set"
	case prev == nil:
		return modeFull, "no cache for this repo"
	case now.Sub(prev.LastFull) >= reconcileInterval:
		return modeFull, fmt.Sprintf("last full fetch %v is older than %v", prev.LastFull.Format(time.RFC3339), reconcileInterval)
	default:
		return modeIncremental, fmt.Sprintf("cache fetched %v, last full fetch %v",
			prev.FetchedAt.Format(time.RFC3339), prev.LastFull.Format(time.RFC3339))
	}
}

// lookupFunc fetches a single item by number. found is false when the item
// is genuinely gone from the repo (deleted, transferred out, hidden).
type lookupFunc func(number int) (it item, found bool, err error)

// reconcile compares a previous cache against a fresh full listing. Cached
// items missing from fresh are looked up one by one: those that still exist
// in this repo were silently dropped by the listing and are added back to
// fresh; the rest are gone. It fails without changing fresh if more than
// limit items are missing.
func reconcile(cached, fresh map[int]item, lookup lookupFunc, limit int) (recovered, dropped int, err error) {
	var missing []int
	for n := range cached {
		if _, ok := fresh[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > limit {
		return 0, 0, fmt.Errorf("%v cached items are missing from the full listing (limit %v); the listing looks incomplete", len(missing), limit)
	}
	slices.Sort(missing)
	found := make([]item, 0, len(missing))
	for _, n := range missing {
		it, ok, err := lookup(n)
		if err != nil {
			return 0, 0, fmt.Errorf("looking up #%v: %w", n, err)
		}
		if ok {
			found = append(found, it)
		} else {
			dropped++
		}
	}
	upsert(fresh, found)
	return len(found), dropped, nil
}

// checkShrink guards a fetch made without a cache against a previously
// written data file: if the latest open total fell below 90% of the old
// file's, the fetch is more likely incomplete than the repo suddenly
// cleaned up.
func checkShrink(old, updated timeline) error {
	oldTotal, newTotal := old.latestTotal(), updated.latestTotal()
	if newTotal*10 < oldTotal*9 {
		return fmt.Errorf("latest open issues+PRs dropped from %v to %v (below 90%%); refusing to overwrite data without a cache to reconcile against", oldTotal, newTotal)
	}
	return nil
}
