// Command github-timeline downloads the issues and pull requests of a GitHub
// repo and writes a per-day count of open issues and open PRs to
// data/{org}/{repo}.json.
//
// Usage:
//
//	go run main.go org/repo
//
// Everything lives in this one file so that `go run main.go` keeps working.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofri/go-github-ratelimit/github_ratelimit"
	"github.com/google/go-github/v88/github"
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

	// retryBackoff is the delay before the first retry; it doubles after each.
	retryBackoff = time.Second

	// fetchTimeout caps how long fetching a single repo's issues may take, so
	// a stuck connection can't hang the job indefinitely.
	fetchTimeout = 50 * time.Minute
)

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("expected the Github repo path as one argument. `go run main.go example-org/example-repo`")
	}
	path := strings.Split(os.Args[1], "/")
	if len(path) != 2 {
		log.Fatalf("expected Github repo path to contain one slash")
	}
	if err := run(path[0], path[1]); err != nil {
		log.Fatalf("%v/%v: %v", path[0], path[1], err)
	}
}

func run(org, repo string) error {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	// Let go-github sleep through (rather than error on) primary rate limits.
	ctx = context.WithValue(ctx, github.SleepUntilPrimaryRateLimitResetWhenRateLimited, true)

	client, err := newClient()
	if err != nil {
		return err
	}
	issues, err := listIssues(ctx, client, org, repo, github.IssueListByRepoOptions{State: "all"})
	if err != nil {
		return err
	}
	if len(issues) == 0 {
		return fmt.Errorf("no issues returned; refusing to overwrite data with an empty timeline")
	}
	items := make([]item, len(issues))
	for i, issue := range issues {
		items[i] = itemFromIssue(issue)
	}

	now := time.Now().UTC()
	tl := buildTimeline(items, now)
	tl.Updated = now
	return writeFileAtomic(filepath.Join("data", org, repo+".json"), encodeTimeline(tl))
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
