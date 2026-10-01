'use strict';

// Diagnostic only. Run in a disposable standard Node image, never an application
// checkout. This does not patch passwd, alter application tests, or set HOME.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const { spawnSync } = require('node:child_process');

function describe(fn) {
  try { return { value: fn() }; }
  catch (error) {
    return { error: { code: error.code, syscall: error.info?.syscall, uvCode: error.info?.code } };
  }
}

const uid = process.geteuid();
const passwdLines = fs.readFileSync('/etc/passwd', 'utf8').split('\n')
  .filter(line => line.split(':')[2] === String(uid));
const result = {
  node: process.version,
  libuv: process.versions.uv,
  uid,
  gid: process.getegid(),
  groups: process.getgroups(),
  homePresent: Object.hasOwn(process.env, 'HOME'),
  home: process.env.HOME,
  passwdLines,
  homedir: describe(() => os.homedir()),
  userInfo: describe(() => os.userInfo()),
};

// Start the same Node binary with an intentionally minimal environment. This
// reproduces a child removing HOME even when the parent has a valid HOME.
const child = spawnSync(process.execPath, ['-e', `
  try {
    console.log(JSON.stringify({ value: require('node:os').homedir() }));
  } catch (error) {
    console.log(JSON.stringify({ error: { code: error.code, syscall: error.info?.syscall, uvCode: error.info?.code } }));
  }
`], { env: {}, encoding: 'utf8', timeout: 10_000 });
if (child.error) throw child.error;
assert.equal(child.status, 0, child.stderr);
result.sanitizedChildHomedir = JSON.parse(child.stdout);
console.log(JSON.stringify(result, null, 2));

// Optional expectations for the published reproduction matrix, not a general
// assumption about NSS: an image may use a passwd source other than this file.
if (process.argv[2] === 'missing-passwd') {
  assert.equal(passwdLines.length, 0);
  const failure = { code: 'ERR_SYSTEM_ERROR', syscall: 'uv_os_homedir', uvCode: 'ENOENT' };
  assert.deepEqual(result.sanitizedChildHomedir.error, failure);
  if (result.homePresent) assert.equal(result.homedir.value, result.home);
  else assert.deepEqual(result.homedir.error, failure);
} else if (process.argv[2] === 'known-passwd') {
  assert.ok(passwdLines.length > 0);
  assert.equal(result.sanitizedChildHomedir.value, passwdLines[0].split(':')[5]);
  if (result.homePresent) assert.equal(result.homedir.value, result.home);
  else assert.equal(result.homedir.value, passwdLines[0].split(':')[5]);
} else if (process.argv[2] !== undefined) {
  throw new Error('Expected missing-passwd, known-passwd, or no assertion mode');
}
