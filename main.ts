// main.ts — thin launcher for the Go forge binary.
//
// The entrypoint the suite runs is `node /bot/main.ts`. The forge itself is
// a statically linked Linux amd64 Go binary committed in this repository as
// vendored source (RULES.txt: the binary "runs from your repository as
// vendored source"). This launcher only spawns that binary, forwards
// signals to it, and exits with its exit code. It serves no traffic and
// proxies nothing: the binary binds its own ephemeral port, registers it
// with the env, and answers every request itself.
//
// Startup robustness: a binary committed via the web UI loses its exec bit
// (mode 100644), and the server checkout may be read-only or owned by
// another user, so chmod-ing the committed file can fail silently. The
// launcher therefore tries, in order: (1) chmod + spawn the committed
// binary; (2) copy it to /dev/shm with mode 755 and spawn the copy;
// (3) copy it to os.tmpdir() with mode 755 and spawn the copy. Temp
// copies are removed on exit (best effort).

import { spawn, type ChildProcess } from 'node:child_process';
import { chmodSync, copyFileSync, unlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const dir = dirname(fileURLToPath(import.meta.url));
const binary = join(dir, 'forge');

// A binary committed via the web UI loses its exec bit (mode 100644), so
// the launcher tries to make it executable itself before spawning it.
try {
  chmodSync(binary, 0o755);
} catch {
  // Read-only checkout or not the owner — the tmp-copy fallbacks below
  // cover this case.
}

function trySpawn(path: string): Promise<ChildProcess> {
  return new Promise((resolve, reject) => {
    const c = spawn(path, [], {
      stdio: 'inherit',
      env: process.env,
    });
    c.once('spawn', () => resolve(c));
    c.once('error', (err) => reject(err));
  });
}

function stageCopy(destDir: string): string {
  const dest = join(destDir, `ironforge-${process.pid}`);
  copyFileSync(binary, dest);
  chmodSync(dest, 0o755);
  return dest;
}

interface Attempt {
  label: string;
  error: unknown;
}

async function start(): Promise<{ child: ChildProcess; tempPath: string | null }> {
  const attempts: Attempt[] = [];
  // (1) Committed binary, chmod-ed above.
  try {
    return { child: await trySpawn(binary), tempPath: null };
  } catch (err) {
    attempts.push({ label: binary, error: err });
  }
  // (2) Copy to /dev/shm, then (3) copy to os.tmpdir().
  for (const destDir of ['/dev/shm', tmpdir()]) {
    let staged: string | null = null;
    try {
      staged = stageCopy(destDir);
      return { child: await trySpawn(staged), tempPath: staged };
    } catch (err) {
      attempts.push({ label: staged ?? destDir, error: err });
      if (staged !== null) {
        try {
          unlinkSync(staged);
        } catch {
          // Best effort cleanup.
        }
      }
    }
  }
  // All attempts failed.
  for (const a of attempts) {
    process.stderr.write(
      `[entry] failed to start forge from ${a.label}: ${a.error instanceof Error ? a.error.message : String(a.error)}\n`,
    );
  }
  process.stderr.write('[entry] no working forge binary; giving up\n');
  process.exit(1);
}

start().then(({ child, tempPath }) => {
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

  child.on('exit', (code, signal) => {
    if (tempPath !== null) {
      try {
        unlinkSync(tempPath);
      } catch {
        // Best effort cleanup.
      }
    }
    if (signal !== null) {
      process.kill(process.pid, signal);
      return;
    }
    process.exit(code ?? 1);
  });
});
