import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import { checkEntry, scan } from './check-community-boundary.mjs';

const checker = fileURLToPath(new URL('./check-community-boundary.mjs', import.meta.url));
function fixture(t, files) {
  const root = mkdtempSync(path.join(os.tmpdir(), 'gpuflow-boundary-'));
  t.after(() => {
    const resolved = path.resolve(root);
    assert.equal(path.dirname(resolved), path.resolve(os.tmpdir()));
    assert.ok(path.basename(resolved).startsWith('gpuflow-boundary-'));
    rmSync(resolved, { recursive: true, force: true });
  });
  for (const [name, content] of Object.entries(files)) {
    const file = path.join(root, name);
    mkdirSync(path.dirname(file), { recursive: true });
    writeFileSync(file, content);
  }
  return root;
}

test('shared scheduler, capability contracts and existing shared UI clients stay public', () => {
  for (const [name, content] of [
    ['internal/store/fair_scheduler.go', 'package store\nfunc scheduleFairLocked() {}'],
    ['pkg/edition/edition.go', 'const FeatureProjectQuotas = "project_quotas"'],
    ['web/src/App.tsx', 'api("/enterprise/v1/insights"); api("/enterprise/v1/agent-bootstrap"); api("/enterprise/v1/registry/credentials")'],
    ['internal/webui/dist/assets/index.js', 'const features={rbac:false,project_quotas:false,node_maintenance:false}'],
    ['deploy/agent/.env.example', 'GPUFLOW_TOKEN=replace-me'],
    ['scripts/agents.conf.example', 'example'],
    ['deploy/license-customer.json.example', 'example'],
    ['scripts/git-push.credential.xml.example', 'example'],
    ['deploy/cosign.pub', 'public-key'],
    ['docs/enterprise-integration.md', 'Shared extension contracts are public.'],
  ]) assert.deepEqual(checkEntry(name, content), [], name);
});

test('private modules are rejected by path and by renamed Go imports', () => {
  for (const name of ['internal/rbac/rbac.go', 'internal/billing/report.go', 'cmd/gpuflow-commercial/main.go', 'pkg/product/product.go', '.tmp-enterprise-copy/internal/license/license.go', 'patches/community-enterprise-ui.patch']) {
    assert.ok(checkEntry(name).includes('private-product-path'), name);
  }
  assert.ok(checkEntry('internal/other/renamed.go', 'import "gpuflow-commercial/internal/rbac"').includes('private-module-dependency'));
});

test('private screens are rejected in both source and minified embedded assets', () => {
  for (const name of ['web/src/App.tsx', 'internal/webui/dist/assets/index-123.js']) {
    for (const route of ['/enterprise/v1/projects', '/enterprise/v1/usage-report?', '/enterprise/v1/scheduling/decisions?limit=50', '/enterprise/v1/license', '/enterprise/v1/nodes/${node.id}/maintenance']) {
      assert.ok(checkEntry(name, `api("${route}")`).includes('private-product-ui'), `${name}: ${route}`);
    }
  }
  assert.deepEqual(checkEntry('docs/BOUNDARY.md', 'Private /enterprise/v1/projects API'), []);
});

test('deployment candidates reject live settings, keys, runtime data and nested archives', () => {
  for (const name of ['deploy/.env', 'deploy/.env.production', 'deploy/agent.env', 'scripts/agents.conf', 'deploy/license.json', 'deploy/license-customer.json', 'deploy/license-acme.v1.json', 'scripts/git-push.credential.xml', 'scripts/credentials.xml', 'deploy/vendor-private.key', 'scripts/backup.tar.gz', 'deploy/data.sqlite', 'backups/export.json']) {
    assert.ok(checkEntry(name).length, name);
  }
  assert.ok(checkEntry('deploy/renamed.txt', ['-----BEGIN', 'PRIVATE KEY-----\nredacted\n-----END PRIVATE KEY-----'].join(' ')).includes('private-key-content'));
});

test('Git mode scans tracked candidates, while build mode catches untracked copied overlays', t => {
  const root = fixture(t, { 'web/src/App.tsx': 'shared frontend', 'local/private.zip': 'local only' });
  execFileSync('git', ['init', '-q', root]);
  execFileSync('git', ['-C', root, 'add', '--', 'web/src/App.tsx']);
  assert.deepEqual(scan(root).findings, []);
  assert.ok(scan(root, 'source-tree').findings.some(f => f.path === 'local/private.zip'));
  writeFileSync(path.join(root, 'web/src/App.tsx'), 'api("/enterprise/v1/projects")');
  assert.ok(scan(root).findings.some(f => f.reason === 'private-product-ui'));
});

test('failed CLI diagnostics do not disclose sensitive contents', t => {
  const sentinel = 'never-print-this-secret-fixture';
  const root = fixture(t, { 'deploy/.env': `TOKEN=${sentinel}`, 'scripts/private.zip': sentinel });
  const result = spawnSync(process.execPath, [checker, '--delivery-tree', root], { encoding: 'utf8' });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /live-configuration/);
  assert.match(result.stderr, /unexpected-archive/);
  assert.ok(!`${result.stdout}${result.stderr}`.includes(sentinel));
});

test('source mode works without Git and ignores installed build dependencies', t => {
  const root = fixture(t, { 'web/src/App.tsx': 'shared frontend', 'web/node_modules/vendor/private.key': 'dependency fixture' });
  const result = spawnSync(process.execPath, [checker, '--source-tree', root], { encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /1 candidate files/);
  const delivery = spawnSync(process.execPath, [checker, '--delivery-tree', root], { encoding: 'utf8' });
  assert.equal(delivery.status, 1);
  assert.match(delivery.stderr, /generated-or-runtime-directory/);
});
