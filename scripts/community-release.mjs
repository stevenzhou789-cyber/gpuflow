import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { appendFileSync, closeSync, copyFileSync, lstatSync, mkdtempSync, openSync, readFileSync, readdirSync, rmSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const VERSION = /^v(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)$/;
const SHA = /^[a-f0-9]{40}$/;
const DIGEST = /^sha256:[a-f0-9]{64}$/;
const MARKER = 'gpuflow-community-release-v1';
const ensure = (condition, message) => { if (!condition) throw new Error(message); };
const hash = file => createHash('sha256').update(readFileSync(file)).digest('hex');

// Commands use argv, inherited GitHub credentials, and sanitized failures. Never print
// gh/cosign stderr: it can contain credential-bearing URLs or other sensitive data.
export function command(binary, args, options = {}) {
  const result = spawnSync(binary, args, { encoding: 'utf8', maxBuffer: 16 * 1024 * 1024, ...options });
  ensure(!result.error, `${binary} could not be started`);
  return result;
}

export function clients(run = command) {
  const ok = (binary, args, options) => {
    const result = run(binary, args, options);
    ensure(result.status === 0, `${binary} command failed`);
    return result.stdout;
  };
  const api = (endpoint, { method = 'GET', body, missing = false, paginate = false } = {}) => {
    const args = ['api', '--hostname', 'github.com', '--method', method, endpoint];
    if (body !== undefined) args.push('--input', '-');
    if (paginate) args.push('--paginate', '--slurp');
    const result = run('gh', args, body === undefined ? {} : { input: JSON.stringify(body) });
    if (missing && result.status !== 0 && /\(HTTP 404\)/.test(result.stderr || '')) return null;
    ensure(result.status === 0, 'GitHub API request failed');
    try { return JSON.parse(result.stdout); } catch { throw new Error('Invalid GitHub API response'); }
  };
  return {
    api,
    head: () => ok('git', ['rev-parse', 'HEAD']).trim(),
    verifyBlob: (key, bundle, file) => ok('cosign', ['verify-blob', '--key', key, '--bundle', bundle, file]),
    verifyImage: (key, image) => ok('cosign', ['verify', '--key', key, image]),
    upload: (repo, tag, file) => ok('gh', ['release', 'upload', tag, file, '--repo', `github.com/${repo}`]),
    download: (repo, asset, file) => {
      ensure(Number.isSafeInteger(asset.id) && asset.id > 0, 'Invalid release asset ID');
      const fd = openSync(file, 'wx');
      try {
        ok('gh', ['api', '--hostname', 'github.com', `repos/${repo}/releases/assets/${asset.id}`, '--header', 'Accept: application/octet-stream'], { stdio: ['ignore', fd, 'pipe'] });
      } finally { closeSync(fd); }
    },
    imageDigest: ref => {
      const result = run('docker', ['buildx', 'imagetools', 'inspect', ref]);
      if (result.status !== 0) {
        // Buildx resolves a missing manifest to this exact error. Authentication,
        // network, repository access and unrecognized diagnostics fail closed.
        const error = (result.stderr || '').trim();
        if (error === `ERROR: ${ref}: not found`) return null;
        throw new Error('Container manifest lookup failed');
      }
      const digest = /^Digest:\s+(sha256:[a-f0-9]{64})\s*$/m.exec(result.stdout || '')?.[1];
      ensure(digest, 'Container manifest response has no digest');
      return digest;
    },
    promoteImage: (source, target) => ok('docker', ['buildx', 'imagetools', 'create', '--tag', target, source]),
  };
}

export function eventContext(env, event) {
  const ref = env.GITHUB_REF || '';
  ensure(SHA.test(env.GITHUB_SHA || ''), 'Missing or invalid workflow commit SHA');
  if (!ref.startsWith('refs/tags/v')) return { isVersion: false, commitSha: env.GITHUB_SHA };
  const tag = ref.slice('refs/tags/'.length);
  ensure(VERSION.test(tag), 'Version must be vX.Y.Z without leading zeroes or suffixes');
  ensure(env.GITHUB_EVENT_NAME === 'push', 'Version releases require the original tag push event');
  ensure(event.ref === ref && event.created === true && event.deleted === false && event.forced === false && event.before === '0'.repeat(40), 'Version tag must be newly created, never moved or reused');
  ensure(SHA.test(event.after || '') && event.after !== '0'.repeat(40), 'Tag push has no valid after SHA');
  ensure(/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(env.GITHUB_REPOSITORY || ''), 'Invalid GitHub repository');
  ensure(event.repository?.full_name?.toLowerCase() === env.GITHUB_REPOSITORY.toLowerCase(), 'Event repository does not match workflow repository');
  ensure(/^[1-9]\d*$/.test(env.GITHUB_RUN_ID || ''), 'Missing workflow run ID');
  return { isVersion: true, repository: env.GITHUB_REPOSITORY, tag, commitSha: env.GITHUB_SHA, after: event.after, runId: env.GITHUB_RUN_ID };
}

export function pinTag(context, io, expectedObject) {
  const base = `repos/${context.repository}/git`;
  const ref = io.api(`${base}/ref/tags/${context.tag}`);
  ensure(ref.ref === `refs/tags/${context.tag}` && SHA.test(ref.object?.sha || ''), 'Invalid remote tag reference');
  const objectSha = ref.object.sha;
  if (expectedObject) ensure(objectSha === expectedObject, 'Version tag object changed');
  let object = ref.object;
  for (let depth = 0; object.type === 'tag'; depth++) {
    ensure(depth < 16, 'Tag nesting exceeds release limit');
    const tag = io.api(`${base}/tags/${object.sha}`);
    ensure(tag.sha === object.sha && SHA.test(tag.object?.sha || ''), 'Invalid annotated tag object');
    object = tag.object;
  }
  ensure(object.type === 'commit' && object.sha === context.commitSha, 'Tag does not point at the workflow commit');
  ensure(context.after === objectSha || context.after === object.sha, 'Tag no longer matches its original push');
  ensure(io.head() === object.sha, 'Checkout is not the pinned release commit');
  return { ...context, tagObjectSha: objectSha };
}

function marker(body) {
  const match = new RegExp(`<!-- ${MARKER} ([A-Za-z0-9+/=]+) -->`, 'g');
  const matches = [...(body || '').matchAll(match)];
  ensure(matches.length === 1, 'Existing release has no unique workflow ownership marker');
  try { return JSON.parse(Buffer.from(matches[0][1], 'base64').toString('utf8')); }
  catch { throw new Error('Invalid release ownership marker'); }
}

function ownDraft(release, context, manifest) {
  ensure(release.draft === true && release.tag_name === context.tag, 'Existing published releases cannot be replaced');
  const owner = marker(release.body);
  for (const key of ['repository', 'tag', 'tagObjectSha', 'commitSha', 'runId']) {
    ensure(owner[key] === context[key], 'Draft belongs to another tag, commit, or workflow run');
  }
  if (manifest) ensure(JSON.stringify(owner) === JSON.stringify(manifest), 'Draft assets or image differ; retry the failed release job with the original build');
}

// The /releases/tags/{tag} endpoint only promises published releases. Listing with
// the release job's write token also sees drafts, including interrupted uploads.
const releaseByTag = (context, io) => {
  const pages = io.api(`repos/${context.repository}/releases?per_page=100`, { paginate: true });
  ensure(Array.isArray(pages) && pages.every(Array.isArray), 'Invalid release listing');
  const releases = pages.flat().filter(release => release.tag_name === context.tag);
  ensure(releases.length <= 1, 'Multiple releases claim the same version tag');
  return releases[0] || null;
};

export function guard(env, event, io = clients()) {
  const context = eventContext(env, event);
  if (!context.isVersion) return context;
  const pinned = pinTag(context, io);
  const existing = releaseByTag(pinned, io);
  if (existing) ownDraft(existing, pinned);
  return pinned;
}

export function assetNames(tag) {
  ensure(VERSION.test(tag), 'Invalid release version');
  const archives = ['gpuflow-linux-amd64.tar.gz', 'gpuflow-linux-arm64.tar.gz', 'gpuflow-windows-amd64.zip', `gpuflow-deployment-${tag}.tar.gz`];
  const signed = [...archives, 'checksums.txt'];
  return { archives, signed, all: [...signed, ...signed.map(name => `${name}.sigstore.json`), 'cosign.pub'].sort() };
}

export function verifyAssets(directory, tag, io) {
  const names = assetNames(tag);
  ensure(JSON.stringify(readdirSync(directory).sort()) === JSON.stringify(names.all), 'Release directory must contain exactly the required signed assets');
  for (const name of names.all) {
    const stat = lstatSync(path.join(directory, name));
    ensure(stat.isFile() && !stat.isSymbolicLink() && stat.size > 0, 'Release assets must be nonempty regular files');
  }
  const checksums = new Map();
  for (const line of readFileSync(path.join(directory, 'checksums.txt'), 'utf8').trim().split(/\r?\n/)) {
    const parsed = /^([a-f0-9]{64}) [ *](?:\.\/)?([A-Za-z0-9._-]+)$/.exec(line);
    ensure(parsed && names.archives.includes(parsed[2]) && !checksums.has(parsed[2]), 'Invalid, duplicate or unexpected checksum entry');
    checksums.set(parsed[2], parsed[1]);
  }
  ensure(checksums.size === names.archives.length, 'Checksums do not cover every archive');
  for (const name of names.archives) ensure(hash(path.join(directory, name)) === checksums.get(name), 'Release archive checksum mismatch');
  const key = path.join(directory, 'cosign.pub');
  for (const name of names.signed) io.verifyBlob(key, path.join(directory, `${name}.sigstore.json`), path.join(directory, name));
  const payloadHashes = Object.fromEntries([...names.signed, 'cosign.pub'].sort().map(name => [name, hash(path.join(directory, name))]));
  return { names, payloadHashes };
}

function temporary(action) {
  const directory = mkdtempSync(path.join(os.tmpdir(), 'gpuflow-release-'));
  try { return action(directory); }
  finally {
    const resolved = path.resolve(directory);
    ensure(path.dirname(resolved) === path.resolve(os.tmpdir()) && path.basename(resolved).startsWith('gpuflow-release-'), 'Unsafe release temporary directory');
    rmSync(resolved, { recursive: true, force: true });
  }
}

function inspectRemoteAssets(context, release, directory, verified, io, complete) {
  const pages = io.api(`repos/${context.repository}/releases/${release.id}/assets?per_page=100`, { paginate: true });
  ensure(Array.isArray(pages) && pages.every(Array.isArray), 'Invalid release asset listing');
  const assets = pages.flat();
  const names = new Set();
  for (const asset of assets) {
    ensure(verified.names.all.includes(asset.name) && !names.has(asset.name) && asset.state === 'uploaded', 'Draft contains unexpected, duplicate, or incomplete assets');
    names.add(asset.name);
  }
  if (complete) ensure(names.size === verified.names.all.length, 'Remote release assets are incomplete');
  temporary(downloads => {
    // Missing files are local verified candidates; existing files are always
    // downloaded and verified. A valid older signature bundle may differ on retry.
    for (const name of verified.names.all) if (!names.has(name)) copyFileSync(path.join(directory, name), path.join(downloads, name));
    for (const asset of assets) io.download(context.repository, asset, path.join(downloads, asset.name));
    const remote = verifyAssets(downloads, context.tag, io);
    ensure(JSON.stringify(remote.payloadHashes) === JSON.stringify(verified.payloadHashes), 'Existing release asset content differs from the pinned payload');
  });
  return names;
}

export function promoteVersionImage(context, imageName, digest, io) {
  const target = `${imageName}:${context.tag}`;
  const current = io.imageDigest(target);
  ensure(current === null || current === digest, 'Version image already points to another digest');
  if (current === null) io.promoteImage(`${imageName}@${digest}`, target);
  ensure(io.imageDigest(target) === digest, 'Version image promotion did not preserve the signed digest');
}

export function publish(env, event, directory, io = clients()) {
  const context = eventContext(env, event);
  ensure(context.isVersion, 'This publisher only accepts immutable numbered versions');
  ensure(SHA.test(env.EXPECTED_TAG_OBJECT_SHA || '') && env.EXPECTED_COMMIT_SHA === context.commitSha, 'Missing pinned tag and commit outputs from the guard job');
  const pinned = pinTag(context, io, env.EXPECTED_TAG_OBJECT_SHA);
  const imageName = `ghcr.io/${context.repository.toLowerCase()}`;
  ensure(env.IMAGE_NAME === imageName && DIGEST.test(env.IMAGE_DIGEST || ''), 'Missing or invalid release image digest');
  const verified = verifyAssets(directory, context.tag, io);
  io.verifyImage(path.join(directory, 'cosign.pub'), `${imageName}@${env.IMAGE_DIGEST}`);
  const manifest = Object.fromEntries(['repository', 'tag', 'tagObjectSha', 'commitSha', 'runId'].map(key => [key, pinned[key]]));
  manifest.image = `${imageName}@${env.IMAGE_DIGEST}`;
  manifest.payloadHashes = verified.payloadHashes;
  let release = releaseByTag(pinned, io);
  if (release) ownDraft(release, pinned, manifest);
  else {
    pinTag(context, io, pinned.tagObjectSha);
    const body = `GPUFlow Community ${context.tag}\n\nSource commit: ${context.commitSha}\nContainer image: ${manifest.image}\n\n<!-- ${MARKER} ${Buffer.from(JSON.stringify(manifest)).toString('base64')} -->`;
    release = io.api(`repos/${context.repository}/releases`, { method: 'POST', body: { tag_name: context.tag, target_commitish: context.commitSha, name: `GPUFlow Community ${context.tag}`, body, draft: true, prerelease: false } });
    ownDraft(release, pinned, manifest);
  }
  ensure(Number.isSafeInteger(release.id) && release.id > 0, 'Invalid release ID');
  const existingNames = inspectRemoteAssets(pinned, release, directory, verified, io, false);
  for (const name of verified.names.all) if (!existingNames.has(name)) {
    pinTag(context, io, pinned.tagObjectSha);
    io.upload(context.repository, context.tag, path.join(directory, name));
  }
  const current = releaseByTag(pinned, io);
  ensure(current?.id === release.id, 'Release identity changed');
  ownDraft(current, pinned, manifest);
  inspectRemoteAssets(pinned, release, directory, verified, io, true);
  pinTag(context, io, pinned.tagObjectSha);
  promoteVersionImage(pinned, imageName, env.IMAGE_DIGEST, io);
  pinTag(context, io, pinned.tagObjectSha);
  const published = io.api(`repos/${context.repository}/releases/${release.id}`, { method: 'PATCH', body: { draft: false, make_latest: 'true' } });
  ensure(published.id === release.id && published.draft === false && published.tag_name === context.tag, 'Release publication was not confirmed; inspect the original run before retrying');
  return published.html_url;
}

function main() {
  const [action, option, directory, ...extra] = process.argv.slice(2);
  ensure(action === 'guard' ? option === undefined : action === 'publish' && option === '--assets-dir' && directory && extra.length === 0, 'Usage: community-release.mjs guard | publish --assets-dir DIR');
  const event = process.env.GITHUB_EVENT_PATH ? JSON.parse(readFileSync(process.env.GITHUB_EVENT_PATH, 'utf8')) : {};
  if (action === 'publish') console.log(publish(process.env, event, path.resolve(directory)));
  else {
    const result = guard(process.env, event);
    const outputs = { is_version: String(result.isVersion), tag: result.tag || '', tag_object_sha: result.tagObjectSha || '', commit_sha: result.commitSha };
    if (process.env.GITHUB_OUTPUT) appendFileSync(process.env.GITHUB_OUTPUT, Object.entries(outputs).map(([key, value]) => `${key}=${value}\n`).join(''));
    console.log(result.isVersion ? `Validated original push for ${result.tag} at ${result.commitSha}` : 'Numbered-release guard is not applicable to this ref');
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { main(); } catch (error) { console.error(`Community release blocked: ${error.message}`); process.exitCode = 1; }
}
