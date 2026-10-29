import { spawn } from 'node:child_process';
import { cp, lstat, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { unlinkSync } from 'node:fs';
import { setTimeout as delay } from 'node:timers/promises';

const pluginSource = '/opt/openfactory/plugin';
const pluginDir = '/home/bb/.openfactory-plugin';
const ownershipFile = `${pluginDir}/.openfactory-owned`;
const ownership = 'openfactory managed BB plugin\n';
const readyFile = '/tmp/openfactory-plugin-ready';
const serverUrl = 'http://127.0.0.1:38886';

function clearReady() {
  try {
    unlinkSync(readyFile);
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
  }
}

async function checkServer(signal) {
  const response = await fetch(`${serverUrl}/health`, { signal });
  if (!response.ok || (await response.json()).ok !== true) {
    throw new Error('BB health endpoint is not ready');
  }
}

async function checkPlugin(signal) {
  const response = await fetch(`${serverUrl}/api/v1/plugins`, { signal });
  if (!response.ok) throw new Error(`BB plugin list returned HTTP ${response.status}`);
  const { plugins } = await response.json();
  const plugin = plugins.find((entry) => entry.id === 'openfactory');
  if (!plugin || !plugin.enabled || plugin.status !== 'running' ||
      plugin.rootDir !== pluginDir || plugin.source !== `path:${pluginDir}`) {
    throw new Error(`OpenFactory plugin is not running from the packaged path: ${plugin?.statusDetail ?? plugin?.status ?? 'not installed'}`);
  }
}

async function refreshPlugin() {
  const existing = await lstat(pluginDir).catch((error) => {
    if (error.code !== 'ENOENT') throw error;
    return null;
  });
  if (existing) {
    // Only replace our own directory; never follow or remove a user symlink.
    if (!existing.isDirectory() || existing.isSymbolicLink() ||
        await readFile(ownershipFile, 'utf8').catch(() => '') !== ownership) {
      throw new Error(`Refusing to replace unowned plugin path ${pluginDir}`);
    }
    await rm(pluginDir, { recursive: true });
  }
  await mkdir(pluginDir, { mode: 0o700 });
  // Mark before copying so a copy interrupted by container shutdown is recoverable.
  await writeFile(ownershipFile, ownership, { mode: 0o600 });
  await cp(pluginSource, pluginDir, { recursive: true, force: true, verbatimSymlinks: true });
}

async function start() {
  clearReady();
  let stopping = false;
  let stopSignal = 'SIGTERM';
  let failed = false;
  let interrupt;
  const interrupted = new Promise((resolve) => { interrupt = resolve; });
  const children = [];
  const terminate = (signal) => {
    for (const { child } of children) {
      if (child.exitCode === null && child.signalCode === null) child.kill(signal);
    }
  };
  const onSignal = (signal) => {
    stopping = true;
    stopSignal = signal;
    clearReady();
    interrupt(new Error(`Startup interrupted by ${signal}`));
    // Signal only the launcher: BB owns graceful server/host-daemon shutdown.
    terminate(signal);
  };
  const sigint = () => onSignal('SIGINT');
  const sigterm = () => onSignal('SIGTERM');
  process.on('SIGINT', sigint);
  process.on('SIGTERM', sigterm);

  function launch(command, args, env = process.env) {
    const child = spawn(command, args, { stdio: 'inherit', env });
    const exited = new Promise((resolve) => {
      child.once('error', (error) => resolve({ code: 1, error }));
      child.once('exit', (code, signal) => resolve({ code, signal }));
    });
    const managed = { child, exited };
    children.push(managed);
    return managed;
  }

  async function untilInterrupted(promise, timeoutMs) {
    let timer;
    try {
      return await Promise.race([
        promise,
        interrupted.then((error) => { throw error; }),
        new Promise((_, reject) => {
          timer = setTimeout(() => reject(new Error('BB plugin startup timed out')), timeoutMs);
        }),
      ]);
    } finally {
      clearTimeout(timer);
    }
  }

  try {
    await refreshPlugin();
    if (stopping) return;
    const [command, ...args] = process.argv.slice(2);
    if (!command) throw new Error('BB startup command is required');
    const server = launch(command, args);
    void server.exited.then((result) => {
      clearReady();
      interrupt(result.error ?? new Error(`BB exited during startup (${result.code ?? result.signal})`));
    });
    await untilInterrupted((async () => {
      const deadline = Date.now() + 180_000;
      while (Date.now() < deadline && !stopping) {
        try {
          await checkServer(AbortSignal.timeout(4_000));
          return;
        } catch {
          await delay(1_000);
        }
      }
      throw new Error('BB did not become ready for plugin installation');
    })(), 180_000);

    // Path installs rebuild, dispose and reload an existing registration. Do not
    // edit BB's registry or remove its data: settings survive image restarts.
    const installer = launch('bb', ['plugin', 'install', pluginDir, '--yes', '--json'], {
      ...process.env,
      BB_SERVER_URL: serverUrl,
    });
    const result = await untilInterrupted(installer.exited, 180_000);
    if (result.error) throw result.error;
    if (result.code !== 0) throw new Error(`OpenFactory plugin installation failed (${result.code ?? result.signal})`);
    await untilInterrupted(checkPlugin(AbortSignal.timeout(4_000)), 5_000);
    await writeFile(readyFile, 'ready\n', { mode: 0o600 });
    if (stopping) return;
    const exit = await server.exited;
    if (exit.error) throw exit.error;
    if (!stopping && exit.code !== 0) {
      throw new Error(`BB exited (${exit.code ?? exit.signal})`);
    }
  } catch (error) {
    if (!stopping) {
      failed = true;
      console.error(`BB OpenFactory startup: ${error.message}`);
    }
  } finally {
    clearReady();
    stopping = true;
    terminate(stopSignal);
    await Promise.all(children.map(({ exited }) => exited));
    process.off('SIGINT', sigint);
    process.off('SIGTERM', sigterm);
    if (failed) process.exitCode = 1;
  }
}

try {
  if (process.argv[2] === '--healthcheck') {
    await readFile(readyFile);
    const signal = AbortSignal.timeout(4_000);
    await checkServer(signal);
    await checkPlugin(signal);
  } else {
    await start();
  }
} catch (error) {
  console.error(`BB OpenFactory startup: ${error.message}`);
  process.exitCode = 1;
}
