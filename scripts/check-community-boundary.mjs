#!/usr/bin/env node
// Product-boundary guard, not a replacement for credential or dependency scans.
// Shared scheduling algorithms, capability names and extension contracts are
// intentionally public. Reject private product modules and complete UI overlays.
import { execFileSync } from 'node:child_process';
import { lstatSync, readdirSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const privateModulePath = /(?:^|\/)(?:cmd\/(?:gpuflow-commercial|licensegen)|internal\/(?:audit|billing|insights|license|nodeapi|projectapi|projects|rbac|registry|schedulingapi|upgradecompat)|pkg\/product)(?:\/|$)/i;
const privateWorkingPath = /(?:^|\/)(?:gpuflow-commercial|gpuflow-license-issuer|\.(?:enterprise|review-enterprise|tmp-enterprise)[^/]*)(?:\/|$)/i;
const privateUIPath = /(?:^|\/)(?:community-enterprise-ui\.patch|(?:apply|prepare|test)-enterprise-ui\.[^/]+)$/i;
// These routes belong to private, complete product screens. The shared UI's
// insights, bootstrap and registry credential clients remain permitted.
const privateUIRoute = /\/enterprise\/v1\/(?:projects|scheduling\/(?:projects|decisions)|usage-report|license|audit)(?=[/\s?"'`\\]|$)|\/enterprise\/v1\/nodes\/[^\s"'`]*\/maintenance/;

export function checkEntry(relativePath, content = '') {
  const name = relativePath.replaceAll('\\', '/');
  const reasons = [];
  if (name.startsWith('/') || name.split('/').includes('..')) reasons.push('unsafe-relative-path');
  if (privateModulePath.test(name) || privateWorkingPath.test(name) || privateUIPath.test(name)) reasons.push('private-product-path');
  const basename = name.split('/').at(-1);
  const example = /(?:\.example|\.sample|\.template)$/i.test(basename);
  if (!example && (/^\.env(?:\.|$)/i.test(basename) || /\.env$/i.test(basename) || /^(?:agents\.conf|license(?:-.+)?\.json|credentials(?:\.json)?|id_(?:rsa|dsa|ecdsa|ed25519))$/i.test(basename) || /(?:^|[.-])credentials?\.xml$/i.test(basename))) reasons.push('live-configuration');
  if (/\.(?:key|pem|p12|pfx|jks|keystore)$/i.test(basename) && !/(?:^|[-_.])public\.key$/i.test(basename)) reasons.push('key-material');
  if (/\.(?:zip|tar(?:\.(?:gz|bz2|xz|zst))?|tgz|tbz2|txz|7z|rar|gz|xz|zst)$/i.test(basename)) reasons.push('unexpected-archive');
  if (/\.(?:exe|dll|pdb|db|sqlite3?|dump)$/i.test(basename)) reasons.push('binary-or-runtime-data');
  if (/(?:^|\/)(?:\.git|node_modules|__pycache__|\.pytest_cache|backups?|\.gpuflow|\.go-cache[^/]*|gitlab-artifacts|gitlab-logs)(?:\/|$)/i.test(name)) reasons.push('generated-or-runtime-directory');
  if (/^\s*-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----/m.test(content)) reasons.push('private-key-content');
  if (/(?:\.go$|(?:^|\/)go\.(?:mod|sum)$)/i.test(name) && /(?:gpuflow-commercial|gpuflow-license-issuer)(?:\/|[\s"'])/.test(content)) reasons.push('private-module-dependency');
  if (/(?:^|\/)(?:web\/.*\.(?:[cm]?[jt]sx?|html)|internal\/webui\/dist\/.*\.(?:[cm]?js|html))$/i.test(name) && privateUIRoute.test(content)) reasons.push('private-product-ui');
  return [...new Set(reasons)];
}

function treeFiles(root, sourceTree, directory = '') {
  const result = [];
  for (const entry of readdirSync(path.join(root, directory), { withFileTypes: true })) {
    // Build dependencies are not copied into Go or release payloads. In tracked
    // mode they are never skipped and are rejected if accidentally committed.
    if (sourceTree && entry.isDirectory() && ['.git', 'node_modules'].includes(entry.name)) continue;
    const relative = path.posix.join(directory, entry.name);
    if (entry.isDirectory()) result.push(...treeFiles(root, sourceTree, relative));
    else result.push(relative);
  }
  return result;
}

export function scan(root, mode = 'tracked') {
  root = path.resolve(root);
  const entries = mode === 'tracked'
    ? execFileSync('git', ['-c', `safe.directory=${root.replaceAll('\\', '/')}`, '-C', root, 'ls-files', '-z'], { encoding: 'utf8', maxBuffer: 16 * 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe'] }).split('\0').filter(Boolean)
    : treeFiles(root, mode === 'source-tree');
  if (entries.length === 0) throw new Error('No candidate files to check.');
  const findings = [];
  for (const relative of entries) {
    const reasons = checkEntry(relative);
    try {
      const file = path.join(root, relative);
      const stat = lstatSync(file);
      if (stat.isSymbolicLink()) reasons.push('symlink-not-allowed');
      else if (!stat.isFile()) reasons.push('non-file-entry');
      else if (stat.size > 16 * 1024 * 1024) reasons.push('oversized-source-file');
      else reasons.push(...checkEntry(relative, readFileSync(file, 'utf8')));
    } catch {
      reasons.push('unreadable-candidate');
    }
    for (const reason of new Set(reasons)) findings.push({ path: relative, reason });
  }
  return { count: entries.length, findings };
}

function main(args) {
  let mode = 'tracked';
  let root = process.cwd();
  if (args.length) {
    if (args.length !== 2 || !['--tracked', '--source-tree', '--delivery-tree'].includes(args[0])) throw new Error('Usage: node scripts/check-community-boundary.mjs [--tracked|--source-tree|--delivery-tree ROOT]');
    mode = args[0].slice(2);
    root = args[1];
  }
  const { count, findings } = scan(root, mode);
  if (findings.length) {
    // Never print file contents, excerpts or parser/tool errors containing them.
    for (const finding of findings) console.error(`${finding.reason}: ${JSON.stringify(finding.path)}`);
    console.error(`Community boundary check failed: ${findings.length} finding(s).`);
    process.exitCode = 1;
  } else console.log(`Community boundary check passed (${count} candidate files, ${mode} mode).`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { main(process.argv.slice(2)); }
  catch { console.error('Community boundary check could not inspect its candidates; verify the root, mode and Git checkout.'); process.exitCode = 1; }
}
