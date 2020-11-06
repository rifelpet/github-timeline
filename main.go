package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-github/v32/github"
	"golang.org/x/oauth2"
)

type datapoint struct {
	Day        time.Time `json:"day"`
	OpenIssues int       `json:"open_issues"`
	OpenPRs    int       `json:"open_prs"`
}

type report struct {
	Timeline []datapoint `json:"timeline"`
}

// keep it simple
const daysPerMonth = 30

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

	token := os.Getenv("GITHUB_TOKEN")
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	ctx := context.Background()
	tc := oauth2.NewClient(ctx, ts)

	client := github.NewClient(tc)

	opt := &github.IssueListByRepoOptions{
		State: "all",
		ListOptions: github.ListOptions{
			PerPage: 50,
		},
	}
	var allIssues []*github.Issue
	for {
		issues, resp, err := client.Issues.ListByRepo(ctx, org, repo, opt)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Fetched page %v / %v\n", resp.NextPage-1, resp.LastPage-1)
		allIssues = append(allIssues, issues...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
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
	m, err := json.Marshal(r)
	if err != nil {
		log.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join("data", org), os.ModePerm); err != nil {
		log.Fatal(err)
	}

	if err = ioutil.WriteFile(filepath.Join("data", org, fmt.Sprintf("%v.json", repo)), m, 0644); err != nil {
		log.Fatal(err)
	}
}
