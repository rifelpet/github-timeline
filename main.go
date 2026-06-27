package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofri/go-github-pagination/githubpagination"
	"github.com/gofri/go-github-ratelimit/github_ratelimit"
	"github.com/google/go-github/v88/github"
)

const (
	// perPage is the GitHub API maximum page size for the issues endpoint.
	// Larger pages mean fewer requests, which is both faster and less likely
	// to hit a transient failure mid-pagination.
	perPage = 100

	// maxRetries bounds how many times a single page fetch is retried before
	// giving up. GitHub occasionally returns transient 401/403/5xx responses
	// on long-running paginated jobs; retrying avoids losing the whole run.
	maxRetries = 6

	// fetchTimeout caps how long fetching a single repo's issues may take, so
	// a stuck connection can't hang the job indefinitely.
	fetchTimeout = 50 * time.Minute
)

type datapoint struct {
	Day        time.Time `json:"day"`
	OpenIssues int       `json:"open_issues"`
	OpenPRs    int       `json:"open_prs"`
}

type report struct {
	Timeline []datapoint `json:"timeline"`
}

// listByRepoWithRetry fetches a single page of issues, retrying transient
// failures (network errors and 401/403/429/5xx responses) with exponential
// backoff. GitHub intermittently returns these on long paginated jobs, and
// without retries a single blip aborts the entire scrape.
func listByRepoWithRetry(ctx context.Context, client *github.Client, org, repo string, opt *github.IssueListByRepoOptions) ([]*github.Issue, *github.Response, error) {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			log.Printf("retrying page fetch for %v/%v (attempt %v/%v) after %v: %v", org, repo, attempt+1, maxRetries, backoff, lastErr)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		issues, resp, err := client.Issues.ListByRepo(ctx, org, repo, opt)
		if err == nil {
			return issues, resp, nil
		}
		if !isRetryable(err) {
			return nil, nil, err
		}
		lastErr = err
	}
	return nil, nil, fmt.Errorf("exhausted %v retries: %w", maxRetries, lastErr)
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
	var errResp *github.ErrorResponse
	if errors.As(err, &errResp) && errResp.Response != nil {
		switch errResp.Response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
			return true
		}
		return errResp.Response.StatusCode >= 500
	}
	// No structured HTTP response: most likely a transport/network error. Retry.
	return true
}

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("expected the Github repo path as one argument. `go run main.go example-org/example-repo`")
	}
	path := strings.Split(os.Args[1], "/")
	if len(path) != 2 {
		log.Fatalf("expected Github repo path to contain one slash")
	}
	org := path[0]
	repo := path[1]

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	// Let go-github sleep through (rather than error on) primary rate limits.
	ctx = context.WithValue(ctx, github.SleepUntilPrimaryRateLimitResetWhenRateLimited, true)

	rateLimiter, err := github_ratelimit.NewRateLimitWaiterClient(nil)
	if err != nil {
		log.Fatalf("failed to create github rate limiter client: %v", err)
	}
	paginator := githubpagination.NewClient(rateLimiter.Transport,
		githubpagination.WithPerPage(perPage),
		githubpagination.WithPaginationEnabled(),
	)

	opts := []github.ClientOptionsFunc{github.WithHTTPClient(paginator)}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		opts = append(opts, github.WithAuthToken(token))
	}
	client, err := github.NewClient(opts...)
	if err != nil {
		log.Fatalf("failed to create github client: %v", err)
	}

	opt := &github.IssueListByRepoOptions{
		State: "all",
	}
	var allIssues []*github.Issue
	for {
		issues, resp, err := listByRepoWithRetry(ctx, client, org, repo, opt)
		if err != nil {
			log.Fatalf("failed to fetch issues for %v/%v: %v", org, repo, err)
		}
		allIssues = append(allIssues, issues...)
		fmt.Printf("Fetched %v issues for %v/%v so far\n", len(allIssues), org, repo)
		if resp.NextPage == 0 {
			break
		}
		opt.ListOptions.Page = resp.NextPage
	}

	if len(allIssues) == 0 {
		log.Fatalf("no issues returned for %v/%v; refusing to overwrite data with an empty timeline", org, repo)
	}

	oldestTime := allIssues[len(allIssues)-1].CreatedAt
	oldestDay := time.Date(oldestTime.Year(), oldestTime.Month(), oldestTime.Day(), 0, 0, 0, 0, time.UTC)
	now := time.Now().UTC()
	numDays := int(now.Sub(oldestDay).Hours() / 24)

	data := make([]datapoint, numDays+1)
	for i := 0; i < numDays+1; i++ {
		data[i] = datapoint{
			Day: oldestDay.Add(time.Duration(i*24) * time.Hour),
		}
	}
	for _, issue := range allIssues {
		createdDay := time.Date(issue.CreatedAt.Year(), issue.CreatedAt.Month(), issue.CreatedAt.Day(), 0, 0, 0, 0, time.UTC)
		closedDay := now
		if issue.ClosedAt != nil {
			closedDay = time.Date(issue.ClosedAt.Year(), issue.ClosedAt.Month(), issue.ClosedAt.Day(), 0, 0, 0, 0, time.UTC)
		}
		for d := createdDay; !d.After(closedDay); d = d.AddDate(0, 0, 1) {
			index := int(d.Sub(oldestDay).Hours() / 24)
			if issue.IsPullRequest() {
				data[index].OpenPRs++
			} else {
				data[index].OpenIssues++
			}
		}
	}
	r := report{
		Timeline: data,
	}
	m, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join("data", org), os.ModePerm); err != nil {
		log.Fatal(err)
	}

	if err = os.WriteFile(filepath.Join("data", org, fmt.Sprintf("%v.json", repo)), m, 0644); err != nil {
		log.Fatal(err)
	}
}
