'use strict';

const DAY_MS = 24 * 60 * 60 * 1000;

// Old ?repo= values that have since been renamed in repos.json.
const REPO_ALIASES = {
  'terraform-providers/terraform-provider-aws': 'hashicorp/terraform-provider-aws',
};

const SERIES = [
  { column: 'open_issues', label: 'Issues' },
  { column: 'open_prs', label: 'PRs' },
];

let repos = [];
// Incremented for every render; responses for older requests are ignored.
let latestRequest = 0;

const $ = (id) => document.getElementById(id);

class DataError extends Error {
  constructor(repo, status) {
    super(`HTTP ${status}`);
    this.repo = repo;
    this.status = status;
  }
}

function repoPath(repo) {
  return repo.split('/').map(encodeURIComponent).join('/');
}

async function loadRepoList() {
  const response = await fetch('repos.json');
  if (!response.ok) {
    throw new Error(`Could not load repos.json (HTTP ${response.status})`);
  }
  return response.json();
}

function resolveRepo(repo) {
  if (repos.includes(repo)) {
    return repo;
  }
  const alias = REPO_ALIASES[repo];
  if (alias && repos.includes(alias)) {
    return alias;
  }
  return null;
}

async function fetchTimeline(repo) {
  const response = await fetch(`data/${repoPath(repo)}.json`);
  if (!response.ok) {
    throw new DataError(repo, response.status);
  }
  return parseTimeline(await response.json());
}

// Converts the columnar data file into per-series arrays. Dates are built in
// UTC and kept as YYYY-MM-DD strings so the local timezone can't shift them.
function parseTimeline(raw) {
  const startMs = Date.parse(`${raw.start}T00:00:00Z`);
  if (Number.isNaN(startMs) || !Array.isArray(raw.columns) || !Array.isArray(raw.days)) {
    throw new Error('unrecognized data format');
  }
  // The last row is today's partial day.
  const days = raw.days.slice(0, -1);
  const dates = days.map((_, i) => new Date(startMs + i * DAY_MS).toISOString().slice(0, 10));
  const series = {};
  for (const { column } of SERIES) {
    const index = raw.columns.indexOf(column);
    if (index === -1) {
      throw new Error(`missing column "${column}"`);
    }
    series[column] = days.map((row) => row[index]);
  }
  return { dates, series, updated: raw.updated };
}

function showMessage(text, kind = 'warning') {
  const p = document.createElement('p');
  p.className = kind;
  p.textContent = text;
  $('messages').appendChild(p);
}

function describeError(repo, err) {
  if (err instanceof DataError && err.status === 404) {
    return `No data yet for ${repo}. It will appear after the next daily update.`;
  }
  return `Could not load data for ${repo} (${err.message}).`;
}

function setTitle(repo) {
  const title = $('charttitle');
  title.replaceChildren();
  const a = document.createElement('a');
  a.href = `https://github.com/${repoPath(repo)}`;
  a.textContent = repo;
  title.appendChild(a);
}

function setUpdated(updated) {
  const el = $('updated');
  const date = updated ? new Date(updated) : null;
  if (!date || Number.isNaN(date.getTime())) {
    el.textContent = '';
    return;
  }
  const formatted = date.toLocaleString(undefined, {
    timeZone: 'UTC',
    dateStyle: 'medium',
    timeStyle: 'short',
  });
  el.textContent = `Last updated ${formatted} UTC`;
}

function clearChart() {
  Plotly.purge($('graph'));
}

function drawChart(timeline) {
  const traces = SERIES.map(({ column, label }) => ({
    type: 'scatter',
    mode: 'lines',
    name: label,
    x: timeline.dates,
    y: timeline.series[column],
  }));
  const layout = {
    autosize: true,
    margin: { t: 20, r: 10, b: 40, l: 50 },
    legend: { orientation: 'h', y: 1.08 },
    xaxis: { type: 'date' },
    yaxis: { rangemode: 'tozero' },
  };
  Plotly.react($('graph'), traces, layout, { displayModeBar: false, responsive: true });
}

function updateUrl(repo) {
  const params = new URLSearchParams(location.search);
  params.set('repo', repo);
  history.replaceState(null, '', `${location.pathname}?${params}`);
}

async function render() {
  const repo = $('reposelect').value;
  const request = ++latestRequest;
  updateUrl(repo);

  let timeline;
  let error;
  try {
    timeline = await fetchTimeline(repo);
  } catch (err) {
    error = err;
  }
  if (request !== latestRequest) {
    return; // A newer selection has been made since this one started.
  }

  $('messages').replaceChildren();
  setTitle(repo);
  if (error) {
    setUpdated(null);
    clearChart();
    showMessage(describeError(repo, error), error.status === 404 ? 'warning' : 'error');
    return;
  }
  setUpdated(timeline.updated);
  drawChart(timeline);
}

async function init() {
  try {
    repos = await loadRepoList();
  } catch (err) {
    showMessage(err.message, 'error');
    return;
  }

  const select = $('reposelect');
  for (const repo of repos) {
    select.appendChild(new Option(repo, repo));
  }
  const requested = resolveRepo(new URLSearchParams(location.search).get('repo'));
  if (requested) {
    select.value = requested;
  }
  select.addEventListener('change', render);
  render();
}

document.addEventListener('DOMContentLoaded', init);
