import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { assetNames, clients, eventContext, guard, pinTag, promoteVersionImage, publish, verifyAssets } from './community-release.mjs';

const COMMIT = 'a'.repeat(40);
const OBJECT = 'b'.repeat(40);
const DIGEST = `sha256:${'c'.repeat(64)}`;
const OTHER = 'd'.repeat(40);
const TAG = 'v1.0.0';
const REPO = 'example/gpuflow';
const sha256 = value => createHash('sha256').update(value).digest('hex');
const env = () => ({ GITHUB_SHA: COMMIT, GITHUB_REF: `refs/tags/${TAG}`, GITHUB_EVENT_NAME: 'push', GITHUB_REPOSITORY: REPO, GITHUB_RUN_ID: '123', EXPECTED_TAG_OBJECT_SHA: OBJECT, EXPECTED_COMMIT_SHA: COMMIT, IMAGE_NAME: `ghcr.io/${REPO}`, IMAGE_DIGEST: DIGEST });
const event = () => ({ ref: `refs/tags/${TAG}`, created: true, deleted: false, forced: false, before: '0'.repeat(40), after: OBJECT, repository: { full_name: REPO } });

function fixture(t) {
  const directory = mkdtempSync(path.join(os.tmpdir(), 'gpuflow-release-test-'));
  t.after(() => {
    const resolved = path.resolve(directory);
    assert.equal(path.dirname(resolved), path.resolve(os.tmpdir()));
    assert.ok(path.basename(resolved).startsWith('gpuflow-release-test-'));
    rmSync(resolved, { recursive: true, force: true });
  });
  const names = assetNames(TAG);
  for (const name of names.archives) writeFileSync(path.join(directory, name), `deterministic archive fixture ${name}`);
  writeFileSync(path.join(directory, 'checksums.txt'), names.archives.map(name => `${sha256(readFileSync(path.join(directory, name)))}  ./${name}\n`).join(''));
  writeFileSync(path.join(directory, 'cosign.pub'), 'fixture public key');
  for (const name of names.signed) writeFileSync(path.join(directory, `${name}.sigstore.json`), JSON.stringify({ digest: sha256(readFileSync(path.join(directory, name))), key: 'fixture public key', nonce: 'initial' }));
  return directory;
}

// Stateful fake GitHub/registry exercises partial uploads, downloads and retries.
// Signature stubs model cosign success/failure; these tests do not replace cosign.
function server() {
  const state = { objectSha: OBJECT, commitSha: COMMIT, release: null, assets: new Map(), log: [], nextId: 1, image: null, uploadAttempts: 0 };
  const io = {
    head: () => state.checkout || COMMIT,
    api: (endpoint, options = {}) => {
      if (endpoint.endsWith(`/git/ref/tags/${TAG}`)) return { ref: `refs/tags/${TAG}`, object: { type: state.lightweight ? 'commit' : 'tag', sha: state.objectSha } };
      if (endpoint.endsWith(`/git/tags/${state.objectSha}`)) return { sha: state.objectSha, object: { type: 'commit', sha: state.commitSha } };
      if (endpoint.endsWith('/releases?per_page=100')) return [state.release ? [structuredClone(state.release)] : []];
      if (endpoint.endsWith('/releases') && options.method === 'POST') {
        assert.equal(state.release, null);
        state.log.push('draft');
        state.release = { id: 42, html_url: `https://github.com/${REPO}/releases/tag/${TAG}`, ...options.body };
        return structuredClone(state.release);
      }
      if (endpoint.endsWith('/releases/42/assets?per_page=100')) return [[...state.assets].map(([name, asset]) => ({ id: asset.id, name, state: asset.state || 'uploaded' }))];
      if (endpoint.endsWith('/releases/42') && options.method === 'PATCH') {
        assert.equal(state.assets.size, 11);
        state.log.push('publish');
        Object.assign(state.release, options.body);
        return structuredClone(state.release);
      }
      throw new Error(`Unexpected API endpoint: ${endpoint}`);
    },
    verifyBlob: (key, bundle, file) => {
      const data = JSON.parse(readFileSync(bundle, 'utf8'));
      assert.equal(data.digest, sha256(readFileSync(file)), 'invalid blob signature');
      assert.equal(data.key, readFileSync(key, 'utf8'), 'invalid signing key');
      state.log.push('verify-blob');
    },
    verifyImage: (_key, image) => { assert.equal(image, `ghcr.io/${REPO}@${DIGEST}`); state.log.push('verify-image'); },
    upload: (_repo, _tag, file) => {
      state.uploadAttempts++;
      if (state.uploadAttempts === state.failUpload) throw new Error('Simulated upload interruption');
      const name = path.basename(file);
      assert.ok(!state.assets.has(name), 'assets must never be overwritten');
      state.assets.set(name, { id: state.nextId++, bytes: readFileSync(file) });
      state.log.push(`upload:${name}`);
      state.afterUpload?.();
    },
    download: (_repo, asset, file) => {
      state.log.push(`download:${asset.name}`);
      writeFileSync(file, state.assets.get(asset.name).bytes);
    },
    imageDigest: () => state.image,
    promoteImage: (source, target) => {
      assert.equal(source, `ghcr.io/${REPO}@${DIGEST}`);
      assert.equal(target, `ghcr.io/${REPO}:${TAG}`);
      assert.equal(state.assets.size, 11);
      state.log.push('promote');
      state.image = DIGEST;
    },
  };
  return { state, io };
}

test('only canonical versions and original creation push events enter version publication', () => {
  assert.equal(eventContext(env(), event()).tag, TAG);
  for (const tag of ['v01.0.0', 'v1.00.0', 'v1.0.01', 'v1.0', 'v1.0.0-rc.1', 'v1.0.0+build', 'v1.0.0/extra']) {
    assert.throws(() => eventContext({ ...env(), GITHUB_REF: `refs/tags/${tag}` }, { ...event(), ref: `refs/tags/${tag}` }), /Version must/);
  }
  for (const patch of [{ created: false }, { forced: true }, { deleted: true }, { before: COMMIT }, { after: 'bad' }, { ref: 'refs/tags/v2.0.0' }, { repository: { full_name: 'other/repo' } }]) {
    assert.throws(() => eventContext(env(), { ...event(), ...patch }));
  }
  assert.throws(() => eventContext({ ...env(), GITHUB_EVENT_NAME: 'workflow_dispatch' }, event()), /original tag push/);
  for (const ref of ['refs/heads/main', 'refs/pull/1/merge', 'refs/tags/stable']) assert.deepEqual(guard({ ...env(), GITHUB_REF: ref }, {}, {}), { isVersion: false, commitSha: COMMIT });
});

test('guard binds annotated and lightweight tags, payload, checkout and immutable object snapshot', () => {
  const { state, io } = server();
  assert.equal(guard(env(), event(), io).tagObjectSha, OBJECT);
  assert.equal(guard(env(), { ...event(), after: COMMIT }, io).commitSha, COMMIT);
  assert.throws(() => pinTag(eventContext(env(), event()), io, OTHER), /object changed/);
  state.commitSha = OTHER;
  assert.throws(() => guard(env(), event(), io), /workflow commit/);
  state.commitSha = COMMIT;
  state.checkout = OTHER;
  assert.throws(() => guard(env(), event(), io), /Checkout/);
  state.checkout = COMMIT;
  state.objectSha = COMMIT;
  state.lightweight = true;
  assert.equal(guard(env(), { ...event(), after: COMMIT }, io).tagObjectSha, COMMIT);
});

test('all payloads, checksums, and bundles are mandatory before publication', t => {
  const directory = fixture(t);
  const { io } = server();
  assert.equal(Object.keys(verifyAssets(directory, TAG, io).payloadHashes).length, 6);
  writeFileSync(path.join(directory, 'extra.env'), 'fixture');
  assert.throws(() => verifyAssets(directory, TAG, io), /exactly/);
  rmSync(path.join(directory, 'extra.env'));
  writeFileSync(path.join(directory, 'checksums.txt'), `${'a'.repeat(64)}  ../gpuflow-linux-amd64.tar.gz\n`);
  assert.throws(() => verifyAssets(directory, TAG, io), /checksum entry/);
});

test('bad blob signature or container signature prevents every GitHub or registry write', t => {
  const directory = fixture(t);
  const { state, io } = server();
  io.verifyImage = () => { throw new Error('invalid image signature'); };
  assert.throws(() => publish(env(), event(), directory, io), /invalid image signature/);
  assert.equal(state.release, null);
  assert.equal(state.assets.size, 0);
  writeFileSync(path.join(directory, 'checksums.txt.sigstore.json'), '{"digest":"wrong"}');
  assert.throws(() => publish(env(), event(), directory, io), /invalid blob signature/);
  assert.equal(state.release, null);
});

test('fresh release verifies every remote asset before promoting the digest and publishing', t => {
  const directory = fixture(t);
  const { state, io } = server();
  assert.equal(publish(env(), event(), directory, io), `https://github.com/${REPO}/releases/tag/${TAG}`);
  assert.equal(state.release.draft, false);
  assert.equal(state.assets.size, 11);
  assert.ok(state.log.indexOf('verify-image') < state.log.indexOf('draft'));
  assert.equal(state.log.filter(item => item.startsWith('download:')).length, 11);
  assert.ok(state.log.findLastIndex(item => item.startsWith('download:')) < state.log.indexOf('promote'));
  assert.ok(state.log.indexOf('promote') < state.log.indexOf('publish'));
  assert.throws(() => guard(env(), event(), io), /published releases/);
  assert.throws(() => publish(env(), event(), directory, io), /published releases/);
});

test('original run resumes an interrupted draft without replacing existing valid bundles', t => {
  const directory = fixture(t);
  const { state, io } = server();
  state.failUpload = 5;
  assert.throws(() => publish(env(), event(), directory, io), /upload interruption/);
  assert.equal(state.release.draft, true);
  assert.equal(state.assets.size, 4);
  const original = new Map([...state.assets].map(([name, asset]) => [name, Buffer.from(asset.bytes)]));
  for (const name of assetNames(TAG).signed) {
    const bundle = path.join(directory, `${name}.sigstore.json`);
    const data = JSON.parse(readFileSync(bundle, 'utf8'));
    writeFileSync(bundle, JSON.stringify({ ...data, nonce: 'new valid signature on retry' }));
  }
  assert.equal(guard(env(), event(), io).tag, TAG);
  publish(env(), event(), directory, io);
  for (const [name, bytes] of original) assert.deepEqual(state.assets.get(name).bytes, bytes);
  assert.equal(state.log.filter(item => item.startsWith('upload:')).length, 11);
});

test('draft ownership and original payload are checked before resuming an upload', t => {
  const directory = fixture(t);
  const { state, io } = server();
  state.failUpload = 1;
  assert.throws(() => publish(env(), event(), directory, io));
  assert.throws(() => guard({ ...env(), GITHUB_RUN_ID: '456' }, event(), io), /another tag, commit, or workflow/);
  assert.throws(() => publish({ ...env(), EXPECTED_TAG_OBJECT_SHA: OTHER }, event(), directory, io), /object changed/);
  writeFileSync(path.join(directory, 'cosign.pub'), 'new key');
  for (const name of assetNames(TAG).signed) {
    const file = path.join(directory, `${name}.sigstore.json`);
    const data = JSON.parse(readFileSync(file, 'utf8'));
    writeFileSync(file, JSON.stringify({ ...data, key: 'new key' }));
  }
  assert.throws(() => publish(env(), event(), directory, io), /Draft assets or image differ/);
  assert.equal(state.assets.size, 0);
});

test('tampered or unexpected remote assets stop resume without uploading or publishing', t => {
  const directory = fixture(t);
  const { state, io } = server();
  state.failUpload = 3;
  assert.throws(() => publish(env(), event(), directory, io));
  const attempts = state.uploadAttempts;
  const asset = state.assets.get('checksums.txt');
  const original = asset.bytes;
  asset.bytes = Buffer.from('tampered');
  assert.throws(() => publish(env(), event(), directory, io), /checksum entry/);
  assert.equal(state.uploadAttempts, attempts);
  asset.bytes = original;
  state.assets.set('unexpected.env', { id: 999, bytes: Buffer.from('fixture') });
  assert.throws(() => publish(env(), event(), directory, io), /unexpected, duplicate, or incomplete/);
  assert.equal(state.release.draft, true);
  assert.equal(state.image, null);
});

test('tag movement during upload blocks promotion and final publication', t => {
  const directory = fixture(t);
  const { state, io } = server();
  state.afterUpload = () => { state.objectSha = OTHER; };
  assert.throws(() => publish(env(), event(), directory, io), /object changed/);
  assert.equal(state.assets.size, 1);
  assert.equal(state.image, null);
  assert.equal(state.release.draft, true);
});

test('version image promotion never overwrites another digest and can resume an identical alias', () => {
  const context = { tag: TAG };
  const { state, io } = server();
  state.image = DIGEST;
  promoteVersionImage(context, `ghcr.io/${REPO}`, DIGEST, io);
  assert.ok(!state.log.includes('promote'));
  state.image = `sha256:${'e'.repeat(64)}`;
  assert.throws(() => promoteVersionImage(context, `ghcr.io/${REPO}`, DIGEST, io), /another digest/);
  let promoted = false;
  assert.throws(() => promoteVersionImage(context, `ghcr.io/${REPO}`, DIGEST, { imageDigest: () => null, promoteImage: () => { promoted = true; } }), /did not preserve/);
  assert.equal(promoted, true);
});

test('CLI adapters distinguish missing objects from API/auth/network errors and never clobber', () => {
  const target = `ghcr.io/${REPO}:${TAG}`;
  const missing = clients(() => ({ status: 1, stderr: `ERROR: ${target}: not found`, stdout: '' }));
  assert.equal(missing.imageDigest(target), null);
  for (const stderr of ['ERROR: authentication required', 'ERROR: unexpected status: 403 Forbidden', 'ERROR: connection timed out', 'ERROR: arbitrary 404 not found']) {
    assert.throws(() => clients(() => ({ status: 1, stderr, stdout: '' })).imageDigest(target), /lookup failed/);
  }
  assert.equal(clients(() => ({ status: 0, stderr: '', stdout: `Name: ${target}\nDigest: ${DIGEST}\n` })).imageDigest(target), DIGEST);
  assert.equal(clients(() => ({ status: 1, stderr: 'gh: Not Found (HTTP 404)' })).api('repos/example/repo/releases/tags/v1.0.0', { missing: true }), null);
  assert.throws(() => clients(() => ({ status: 1, stderr: 'gh: forbidden (HTTP 403)' })).api('endpoint', { missing: true }), /request failed/);
  const calls = [];
  clients((binary, args) => { calls.push({ binary, args }); return { status: 0, stdout: '' }; }).upload(REPO, TAG, 'fixture.tar.gz');
  assert.ok(!calls[0].args.includes('--clobber'));
});
