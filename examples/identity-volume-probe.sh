#!/usr/bin/env bash
# Local Linux permission diagnostic, NOT a Kubernetes or actual DinD test.
# Uses a disposable named volume and a fake Unix socket; no host runtime socket
# is mounted inside any container and no custom image is built.
set -euo pipefail
case "$(docker context inspect default --format '{{.Endpoints.docker.Host}}')" in
  unix://*) ;;
  *) echo 'Expected a local Unix-socket Docker context' >&2; exit 1 ;;
esac
image=node@sha256:367679cf9792759492a486e4aa4b421764d71a9546a6dae8aab81a99eb797b3e
volume="fj-model-perms-$$"
server=''
cleanup() {
  if [[ -n "$server" ]]; then docker --context default rm -f "$server" >/dev/null; fi
  docker --context default volume rm "$volume" >/dev/null
}
docker --context default volume create "$volume" >/dev/null
trap cleanup EXIT
common=(--network none --cap-drop ALL --security-opt no-new-privileges --group-add 10001)
# Model existing emptyDir+fsGroup roots (02777/root:10001), not new hardening.
# Docker's default volume permissions differ; this is not a real kubelet test.
docker --context default run --rm "${common[@]}" --user 0:0 \
  --mount "type=volume,src=$volume,dst=/shared" "$image" \
  sh -c 'chgrp 10001 /shared && chmod 2777 /shared'
server=$(docker --context default run -d --rm "${common[@]}" --user 0:0 \
  --mount "type=volume,src=$volume,dst=/shared" "$image" node -e '
const fs=require("fs");
const s=require("net").createServer(c=>c.end("ok"));
s.listen("/shared/probe.sock",()=>fs.chmodSync("/shared/probe.sock",0o660));')
ready=false
for i in $(seq 1 30); do
  if docker --context default exec "$server" sh -c 'test -S /shared/probe.sock && test "$(stat -c %a /shared/probe.sock)" = 660'; then ready=true; break; fi
  sleep 0.1
done
if [[ "$ready" != true ]]; then echo 'Fake socket failed to start' >&2; exit 1; fi
for user in 0:0 node 10001 23456:34567; do
  echo "--- group-owned volume/socket, user=$user ---"
  docker --context default run --rm "${common[@]}" --user "$user" \
    --mount "type=volume,src=$volume,dst=/shared" \
    --mount "type=volume,src=$volume,dst=/workspace" "$image" node -e '
const assert=require("node:assert/strict"),fs=require("node:fs"),net=require("node:net");
const name="probe-"+process.geteuid();
fs.writeFileSync("/shared/"+name,"shared");
assert.equal(fs.readFileSync("/workspace/"+name,"utf8"),"shared");
fs.writeFileSync("/workspace/"+name,"workspace");
assert.equal(fs.readFileSync("/shared/"+name,"utf8"),"workspace");
const dir=fs.statSync("/shared"),sock=fs.statSync("/shared/probe.sock");
assert.equal(dir.mode&0o7777,0o2777);
assert.equal(sock.mode&0o777,0o660);
assert.equal(sock.gid,10001);
const c=net.connect("/shared/probe.sock"); let got="";
c.on("data",d=>got+=d);
c.on("error",e=>{console.error(e);process.exitCode=1;});
c.on("end",()=>{
  assert.equal(got,"ok");
  console.log(JSON.stringify({uid:process.geteuid(),gid:process.getegid(),groups:process.getgroups(),directoryMode:(dir.mode&0o7777).toString(8),socketMode:(sock.mode&0o777).toString(8),writes:true,socket:true}));
});
c.setTimeout(5000,()=>{c.destroy();process.exitCode=1;});'
done
