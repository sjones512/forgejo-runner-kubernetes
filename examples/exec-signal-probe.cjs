// Diagnostic only: run from a Forgejo `run:` step on the plugin label, not
// from the Runner host. Does not read job secrets or need npm dependencies.
const fs = require('node:fs');
const { spawn } = require('node:child_process');

const fields = /^(Name|Pid|PPid|NSpid|Uid|Gid|Groups|Cap(Inh|Prm|Eff|Bnd|Amb)|NoNewPrivs|Seccomp|Seccomp_filters):/;
function probe(pid, name) {
  console.log(`${name} pid=${pid}`);
  try {
    const status = fs.readFileSync(`/proc/${pid}/status`, 'utf8');
    console.log(status.split('\n').filter(line => fields.test(line)).join('\n'));
  } catch (e) {
    console.log(`status: unavailable (${e.code})`);
  }
  for (const item of ['attr/current', 'attr/apparmor/current', 'ns/pid', 'ns/user', 'uid_map', 'gid_map']) {
    try {
      const path = `/proc/${pid}/${item}`;
      const value = item.startsWith('ns/') ? fs.readlinkSync(path) : fs.readFileSync(path, 'utf8').trim();
      console.log(`${item}: ${value}`);
    } catch (e) {
      console.log(`${item}: unavailable (${e.code})`);
    }
  }
}

(async () => {
  probe(1, 'container init');
  probe(process.pid, 'exec parent');
  console.log(`node ids: uid=${process.getuid()} euid=${process.geteuid()} gid=${process.getgid()} egid=${process.getegid()} groups=${process.getgroups()}`);
  // Child stays alive at most 5s, including if signaling is denied.
  const child = spawn(process.execPath, ['-e', "process.stdout.write('READY\\n'); setTimeout(() => process.exit(0), 5000)"], {
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let childError;
  child.on('error', e => { childError = e; console.log(`child error: code=${e.code} errno=${e.errno} syscall=${e.syscall}`); });
  const exited = new Promise((resolve) => child.once('exit', (code, signal) => resolve({ code, signal })));
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('child not ready')), 3000);
    child.stdout.once('data', () => { clearTimeout(timer); resolve(); });
    child.once('exit', () => { clearTimeout(timer); reject(new Error('child exited before ready')); });
  });
  probe(child.pid, 'spawned child');
  let sent = false;
  try {
    sent = child.kill('SIGTERM');
    console.log(`ChildProcess.kill(SIGTERM) returned ${sent}`);
  } catch (e) {
    console.log(`ChildProcess.kill(SIGTERM): code=${e.code} errno=${e.errno} syscall=${e.syscall}`);
  }
  const result = await exited;
  console.log(`child exit: ${JSON.stringify(result)}`);
  if (!sent || result.signal !== 'SIGTERM' || childError) process.exitCode = 1;
})().catch(e => { console.error(e); process.exitCode = 1; });
