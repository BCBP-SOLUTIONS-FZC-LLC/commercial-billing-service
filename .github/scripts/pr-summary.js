// Posts or updates one PR comment summarising the CI jobs (ci.yml pr-summary;
// run through actions/github-script so the JavaScript stays out of YAML).
const needs = JSON.parse(process.env.NEEDS_JSON || '{}');

function icon(r) {
  if (r === 'success') return '✅';
  if (r === 'skipped') return '⏭️';
  if (r === 'cancelled') return '🚫';
  return '❌';
}

const out = needs['validate-test']?.outputs || {};
const pct = out.pct ? parseFloat(out.pct).toFixed(1) + '%' : null;
const pctAll = out.pct_all ? parseFloat(out.pct_all).toFixed(1) + '%' : null;

const rows = [
  ['Tests — race, unit, integration, e2e, contract', 'validate-test', null],
  ['Coverage ≥ 96% (internal/ + pkg/)', 'validate-test', pct ? pct + (pctAll ? ' (all: ' + pctAll + ')' : '') : null],
  ['Quality — fmt, tidy, vet, lint, arch, guards, metrics, vuln, Helm', 'validate-quality', null],
  ['Image build + Dockerfile lint', 'build-image', null],
  ['Trivy CVE scan', 'trivy', null],
  ['Smoke tests', 'smoke', null],
];

const table = rows
  .map(function (row) {
    const r = needs[row[1]]?.result || 'skipped';
    return '| ' + icon(r) + ' | ' + row[0] + ' | ' + (row[2] || '`' + r + '`') + ' |';
  })
  .join('\n');

const allPassed = rows.every(function (row) {
  return needs[row[1]]?.result === 'success';
});

const marker = '<!-- ci-pr-summary -->';
const body = [
  marker,
  '## ' + (allPassed ? '✅ All checks passed — ready for review' : '❌ Some checks failed'),
  '',
  '| | Check | Result |',
  '|---|---|---|',
  table,
  '',
  'Production changes need a reviewer from the PCI owner group (HLD §15.4).',
  '📦 Image cached for `linux/amd64` · merges to `main` are pushed to GHCR and signed with Cosign · ' +
    '🔍 [Security tab](https://github.com/' + context.repo.owner + '/' + context.repo.repo + '/security)',
].join('\n');

const { data: comments } = await github.rest.issues.listComments({
  ...context.repo,
  issue_number: context.issue.number,
});
const existing = comments.find(function (c) {
  return c.body?.startsWith(marker);
});

if (existing) {
  await github.rest.issues.updateComment({ ...context.repo, comment_id: existing.id, body });
} else {
  await github.rest.issues.createComment({ ...context.repo, issue_number: context.issue.number, body });
}
