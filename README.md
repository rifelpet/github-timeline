# Github Timeline

This project visualizes open issues and pull requests on github repos over time.

View the frontend hosted on [Github Pages](https://rifelpet.github.io/github-timeline/).

Data is updated daily via scheduled Github actions.

## Contributing

Contributions are welcome.

Adding a new repo involves adding it to the list in `repos.json`. Both the frontend dropdown and the scheduled scrape workflow read from that file. A new repo has no cache yet, so its first scrape downloads its full history.

To run the scraper locally:

```sh
GITHUB_TOKEN=$(gh auth token) go run main.go kubernetes/kops
```

This writes `data/kubernetes/kops.json` and `cache/kubernetes/kops.json`. Run `go test ./...` before sending changes.

## How it works

The scraper (`main.go`) keeps a per-repo cache in `cache/{org}/{repo}.json` with the number, created and closed times, and issue/PR type of every issue and PR. The cache is not committed; the workflow carries it between runs with `actions/cache`.

- **Incremental fetch** (the daily default): fetches only issues and PRs updated since the previous run started, with a one-hour overlap, and merges them into the cache.
- **Full fetch**: downloads everything. This happens when a repo has no cache, when its last full fetch is at least 7 days old, or when the `-full` flag is passed (`go run main.go -full org/repo`, or the `full` input of a manual workflow run). An incremental fetch only sees items whose `updated_at` changed, so it misses items that were deleted, transferred to another repo, converted to discussions or hidden as spam. The weekly full fetch drops those. Any cached item missing from the full listing is fetched on its own: it is dropped if it is really gone and kept if the listing just skipped it. If more than 500 cached items are missing, the run fails and nothing is written.

The timeline is always rebuilt from the whole cache. Without a cache, the scraper refuses to overwrite an existing data file if the latest day's open total would drop below 90% of the old value. It also never writes an empty timeline.

### Data format

`data/{org}/{repo}.json`:

```json
{
  "start": "2014-03-28",
  "updated": "2026-11-02T08:25:21Z",
  "columns": ["open_issues", "open_prs"],
  "days": [
    [4, 0],
    [4, 0],
    [1777, 157]
  ]
}
```

`days[i]` holds the counts for UTC date `start + i` days, in the order given by `columns`. The last row is the day of the scrape, so it covers only part of that day. `updated` is the time the scrape finished. An item counts as open on every day from the day it was created through the day it was closed, including both of those days. Each day is on its own line so that the daily data commits produce readable diffs.

### Known limitations

- GitHub only reports an item's most recent close. An issue that was closed, reopened and closed again counts as open for the whole span from its creation to its last close.

## TODO
- [ ] Show distribution of Issue/PR age via cohorts
