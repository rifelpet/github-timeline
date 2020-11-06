# Github Timeline

This project visualizes open issues and pull requests on github repos over time.

View the frontend hosted on [Github Pages](https://rifelpet.github.io/github-timeline/).

Data is updated daily via scheduled Github actions.

## Contributing

Contributions are welcome.

Adding a new repo to the dropdown involves updating the list in `timeline.js` and the job matrix in `.github/workflows/scrape.yml`.

## TODO
[ ] Only fetch Github data not already saved to files