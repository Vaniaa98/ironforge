// main.ts — thin launcher for the Go forge binary.
//
// The entrypoint the suite runs is `node /bot/main.ts`. The forge itself is
// a statically linked Linux amd64 Go binary committed in this repository as
// vendored source (RULES.txt: the binary "runs from your repository as
// vendored source"). This launcher only spawns that binary, forwards
// signals to it, and exits with its exit code. It serves no traffic and
// proxies nothing: the binary binds its own ephemeral port, registers it
// with the env, and answers every request itself.

import { spawn, type ChildProcess } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const dir = dirname(fileURLToPath(import.meta.url));
const binary = join(dir, 'forge');

const child: ChildProcess = spawn(binary, [], {
  stdio: 'inherit',
  env: process.env,
});

let exiting = false;
function forward(signal: NodeJS.Signals): void {
  if (exiting) return;
  exiting = true;
  try {
    child.kill(signal);
  } catch {
    // child already gone
  }
}
process.on('SIGTERM', () => forward('SIGTERM'));
process.on('SIGINT', () => forward('SIGINT'));

child.on('error', (err) => {
  process.stderr.write(`[entry] failed to start forge binary: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal !== null) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
