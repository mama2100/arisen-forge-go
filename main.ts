// main.ts: the harness lifecycle is delegated to the vendored forge binary (Go, static):
// it binds an ephemeral port, POSTs $MAKEFUN_ENV_URL/register {"port": N}, long-polls
// $MAKEFUN_ENV_URL/done and exits 0 when the trial is over. This Node shim only launches
// it and mirrors its exit code; everything the suite scores lives in the binary.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const binary = process.platform === 'win32' ? join(here, 'forge-win.exe') : join(here, 'forge-linux');

const child = spawn(binary, [], { stdio: 'inherit', cwd: here });
child.on('exit', (code, signal) => {
  if (signal) process.exit(0);
  process.exit(code ?? 0);
});
child.on('error', (err) => {
  process.stderr.write('[entry] spawn failed: ' + err.message + '\n');
  process.exit(1);
});
