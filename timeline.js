'use strict';

const DAY_MS = 24 * 60 * 60 * 1000;

// Old ?repo= values that have since been renamed in repos.json.
const REPO_ALIASES = {
  'terraform-providers/terraform-provider-aws': 'hashicorp/terraform-provider-aws',
};

// Series in each data file. In compare mode every repo gets its own color and
// each series type its own dash style.
const SERIES = [
  { column: 'open_issues', label: 'Issues', dash: 'solid' },
  { column: 'open_prs', label: 'PRs', dash: 'dot' },
];

const COLORS = [
  '#1f77b4', '#ff7f0e', '#2ca02c', '#d62728', '#9467bd',
  '#8c564b', '#e377c2', '#7f7f7f', '#bcbd22', '#17becf',
];

const PLOT_CONFIG = { displayModeBar: false, responsive: true };

let repos = [];
// Successfully loaded timelines, keyed by repo.
const cache = new Map();
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

function repoColor(repo) {
  return COLORS[Math.max(0, repos.indexOf(repo)) % COLORS.length];
}

async function loadRepoList() {
  const response = await fetch('repos.json');
  if (!response.ok) {
    throw new Error(`Could not load repos.json (HTTP ${response.status})`);
  }
  const list = await response.json();
  return list.sort((a, b) => a.localeCompare(b));
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
  if (cache.has(repo)) {
    return cache.get(repo);
  }
  const response = await fetch(`data/${repoPath(repo)}.json`);
  if (!response.ok) {
    throw new DataError(repo, response.status);
  }
  const timeline = parseTimeline(await response.json());
  cache.set(repo, timeline);
  return timeline;
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

// ---- URL state -------------------------------------------------------------

// Reads {repos, log} from the URL. Supports repeated ?repo= params and maps
// renamed repos; falls back to the first repo in the list.
function stateFromUrl() {
  const params = new URLSearchParams(location.search);
  const selected = [];
  for (const value of params.getAll('repo')) {
    const repo = resolveRepo(value);
    if (repo && !selected.includes(repo)) {
      selected.push(repo);
    }
  }
  if (selected.length === 0 && repos.length > 0) {
    selected.push(repos[0]);
  }
  return { repos: selected, log: params.get('log') === '1' };
}

function urlForState(state) {
  const parts = state.repos.map((repo) => `repo=${encodeURIComponent(repo).replace(/%2F/g, '/')}`);
  if (state.log) {
    parts.push('log=1');
  }
  return `${location.pathname}?${parts.join('&')}${location.hash}`;
}

function stateFromControls() {
  const primary = $('reposelect').value;
  const others = [...$('comparelist').querySelectorAll('input:checked')]
    .map((input) => input.value)
    .filter((repo) => repo !== primary);
  return { repos: [primary, ...others], log: $('logscale').checked };
}

function applyStateToControls(state) {
  const [primary, ...others] = state.repos;
  $('reposelect').value = primary;
  for (const input of $('comparelist').querySelectorAll('input')) {
    input.checked = others.includes(input.value);
  }
  $('logscale').checked = state.log;
  syncCompareList();
  if (others.length > 0) {
    $('compare').open = true;
  }
}

// The primary repo is always shown, so its compare checkbox is hidden.
function syncCompareList() {
  const primary = $('reposelect').value;
  for (const label of $('comparelist').querySelectorAll('label')) {
    const input = label.querySelector('input');
    const isPrimary = input.value === primary;
    label.hidden = isPrimary;
    if (isPrimary) {
      input.checked = false;
    }
  }
}

// ---- Rendering -------------------------------------------------------------

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

function setTitle(selected) {
  const title = $('charttitle');
  title.replaceChildren();
  selected.forEach((repo, i) => {
    if (i > 0) {
      title.append(' vs ');
    }
    const a = document.createElement('a');
    a.href = `https://github.com/${repoPath(repo)}`;
    a.textContent = repo;
    title.appendChild(a);
  });
}

function formatUpdated(updated) {
  const date = new Date(updated);
  if (!updated || Number.isNaN(date.getTime())) {
    return null;
  }
  const formatted = date.toLocaleString(undefined, {
    timeZone: 'UTC',
    dateStyle: 'medium',
    timeStyle: 'short',
  });
  return `${formatted} UTC`;
}

function setUpdated(loaded) {
  const times = loaded.map(({ timeline }) => formatUpdated(timeline.updated));
  let text = '';
  if (times.length > 0 && times.every((t) => t && t === times[0])) {
    text = `Last updated ${times[0]}`;
  } else if (times.some(Boolean)) {
    text = 'Last updated: ' + loaded
      .map(({ repo }, i) => (times[i] ? `${repo} ${times[i]}` : null))
      .filter(Boolean)
      .join('; ');
  }
  $('updated').textContent = text;
}

function buildTraces(loaded) {
  const compare = loaded.length > 1;
  const traces = [];
  for (const { repo, timeline } of loaded) {
    SERIES.forEach(({ column, label, dash }, i) => {
      traces.push({
        type: 'scatter',
        mode: 'lines',
        name: compare ? `${repo} ${label}` : label,
        legendgroup: compare ? repo : undefined,
        x: timeline.dates,
        y: timeline.series[column],
        line: compare ? { color: repoColor(repo), dash } : { color: COLORS[i] },
      });
    });
  }
  return traces;
}

function drawChart(loaded, state) {
  const layout = {
    autosize: true,
    // Keep the user's zoom when only the scale changes.
    uirevision: state.repos.join(','),
    margin: { t: 10, r: 10, b: 10, l: 50 },
    legend: { orientation: 'h', x: 0, y: -0.25, yanchor: 'top' },
    hovermode: 'x unified',
    xaxis: {
      type: 'date',
      rangeslider: { visible: true, thickness: 0.08 },
      rangeselector: {
        x: 0,
        y: 1.02,
        buttons: [
          { count: 1, label: '1y', step: 'year', stepmode: 'backward' },
          { count: 3, label: '3y', step: 'year', stepmode: 'backward' },
          { count: 5, label: '5y', step: 'year', stepmode: 'backward' },
          { step: 'all', label: 'All' },
        ],
      },
    },
    yaxis: state.log
      ? { type: 'log', title: { text: 'Open (log scale)' } }
      : { type: 'linear', rangemode: 'tozero', title: { text: 'Open' } },
  };
  Plotly.react($('graph'), buildTraces(loaded), layout, PLOT_CONFIG);
}

async function render(state) {
  const request = ++latestRequest;
  const results = await Promise.allSettled(state.repos.map(fetchTimeline));
  if (request !== latestRequest) {
    return; // A newer selection has been made since this one started.
  }

  const loaded = [];
  $('messages').replaceChildren();
  results.forEach((result, i) => {
    const repo = state.repos[i];
    if (result.status === 'fulfilled') {
      loaded.push({ repo, timeline: result.value });
    } else {
      const err = result.reason;
      showMessage(describeError(repo, err), err.status === 404 ? 'warning' : 'error');
    }
  });

  setTitle(state.repos);
  setUpdated(loaded);
  if (loaded.length === 0) {
    Plotly.purge($('graph'));
    return;
  }
  drawChart(loaded, state);
}

function onControlsChange() {
  syncCompareList();
  const state = stateFromControls();
  history.pushState(null, '', urlForState(state));
  render(state);
}

function buildControls() {
  const select = $('reposelect');
  const list = $('comparelist');
  for (const repo of repos) {
    select.appendChild(new Option(repo, repo));

    const label = document.createElement('label');
    const input = document.createElement('input');
    input.type = 'checkbox';
    input.value = repo;
    const swatch = document.createElement('span');
    swatch.className = 'swatch';
    swatch.style.backgroundColor = repoColor(repo);
    label.append(input, swatch, repo);
    list.appendChild(label);
  }
  $('controls').addEventListener('change', onControlsChange);
  $('controls').addEventListener('submit', (event) => event.preventDefault());
}

async function init() {
  try {
    repos = await loadRepoList();
  } catch (err) {
    showMessage(err.message, 'error');
    return;
  }
  buildControls();

  const state = stateFromUrl();
  applyStateToControls(state);
  // Normalize the URL (e.g. renamed repos) without adding a history entry.
  history.replaceState(null, '', urlForState(state));
  render(state);

  window.addEventListener('popstate', () => {
    const next = stateFromUrl();
    applyStateToControls(next);
    render(next);
  });
}

document.addEventListener('DOMContentLoaded', init);
