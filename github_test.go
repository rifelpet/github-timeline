package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-github/v88/github"
)

func newTestClient(t *testing.T, mux *http.ServeMux) (*github.Client, string) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	old := retryBackoff
	retryBackoff = time.Millisecond
	t.Cleanup(func() { retryBackoff = old })
	base := srv.URL + "/"
	client, err := github.NewClient(github.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	return client, client.BaseURL()
}

func TestListIssuesFollowsCursorAndRetriesPerPage(t *testing.T) {
	mux := http.NewServeMux()
	calls := map[string]int{}
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("per_page") != "100" || q.Get("state") != "all" {
			t.Errorf("unexpected query %v", r.URL.RawQuery)
		}
		key := q.Get("page") + "/" + q.Get("after")
		calls[key]++
		switch key {
		case "/":
			next := fmt.Sprintf("<%vrepos/o/r/issues?state=all&per_page=100&after=CUR1&page=2>; rel=\"next\"", "http://"+r.Host+"/")
			w.Header().Set("Link", next)
			fmt.Fprint(w, `[{"number":2},{"number":1}]`)
		case "2/CUR1":
			if calls[key] == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			fmt.Fprint(w, `[{"number":0}]`)
		default:
			t.Errorf("unexpected page request %v", r.URL.RawQuery)
		}
	})
	client, _ := newTestClient(t, mux)

	issues, err := listIssues(context.Background(), client, "o", "r", github.IssueListByRepoOptions{State: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 3 {
		t.Errorf("got %v issues, want 3", len(issues))
	}
	if calls["/"] != 1 {
		t.Errorf("first page fetched %v times; a later page's failure must not restart the crawl", calls["/"])
	}
	if calls["2/CUR1"] != 2 {
		t.Errorf("second page fetched %v times, want 2 (one failure, one retry)", calls["2/CUR1"])
	}
}

func TestLookupIssue(t *testing.T) {
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/repos/o/r/issues/1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"number":1,"created_at":"2024-01-01T00:00:00Z","closed_at":"2024-01-02T00:00:00Z","repository_url":"%vrepos/o/r","pull_request":{}}`, base)
	})
	mux.HandleFunc("/repos/o/r/issues/2", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repos/o/other/issues/9", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/repos/o/other/issues/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"number":9,"created_at":"2024-01-01T00:00:00Z","repository_url":"%vrepos/o/other"}`, base)
	})
	mux.HandleFunc("/repos/o/r/issues/3", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("/repos/o/r/issues/4", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"This issue was deleted"}`, http.StatusGone)
	})
	mux.HandleFunc("/repos/o/r/issues/5", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
	})
	client, b := newTestClient(t, mux)
	base = b

	tests := []struct {
		number    int
		wantFound bool
		wantErr   bool
	}{
		{number: 1, wantFound: true},
		{number: 2, wantFound: false}, // transferred to another repo
		{number: 3, wantFound: false},
		{number: 4, wantFound: false},
		{number: 5, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.number), func(t *testing.T) {
			it, found, err := lookupIssue(context.Background(), client, "o", "r", base+"repos/o/r", tt.number)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if found && (it.Number != 1 || !it.PR || it.Closed.IsZero()) {
				t.Errorf("item = %+v", it)
			}
		})
	}
}
