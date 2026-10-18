'use strict';

const { createHash } = require('node:crypto');
const { createWriteStream } = require('node:fs');
const { chmod, lstat, mkdtemp, readFile, rename, rm } = require('node:fs/promises');
const path = require('node:path');
const { Readable, Transform } = require('node:stream');
const { pipeline } = require('node:stream/promises');
const { version } = require('./package.json');

async function download(url, destination) {
  const response = await fetch(url, { signal: AbortSignal.timeout(120_000) });
  if (!response.ok || !response.body) {
    throw new Error(`Download failed (${response.status}): ${url}`);
  }
  const hash = createHash('sha256');
  await pipeline(
    Readable.fromWeb(response.body),
    new Transform({
      transform(chunk, encoding, callback) {
        hash.update(chunk);
        callback(null, chunk);
      },
    }),
    createWriteStream(destination, { flags: 'wx' }),
  );
  return hash.digest('hex');
}

async function install() {
  const os = { linux: 'linux', darwin: 'darwin', win32: 'windows' }[process.platform];
  const arch = { x64: 'amd64', arm64: 'arm64' }[process.arch];
  if (!os || !arch) {
    throw new Error(`No vinted binary is available for ${process.platform}/${process.arch}. Supported platforms are Linux, macOS, and Windows on x64 and arm64.`);
  }
  const binary = process.platform === 'win32' ? 'vinted.exe' : 'vinted';
  const archive = `vinted_${version}_${os}_${arch}.${os === 'windows' ? 'zip' : 'tar.gz'}`;
  const base = `https://github.com/Microck/vinted-cli/releases/download/v${version}`;
  // Use the package directory so the final rename stays on the same filesystem.
  const temporary = await mkdtemp(path.join(__dirname, '.install-'));
  try {
    const checksumsPath = path.join(temporary, 'checksums.txt');
    const archivePath = path.join(temporary, archive);
    await download(`${base}/checksums.txt`, checksumsPath);
    const checksums = await readFile(checksumsPath, 'utf8');
    const matches = checksums.split(/\r?\n/).map(line => line.match(/^([a-fA-F0-9]{64})\s+\*?(.+)$/))
      .filter(match => match && match[2] === archive);
    if (matches.length !== 1) {
      throw new Error(`Release v${version} must contain exactly one checksum for ${archive}.`);
    }
    const actual = await download(`${base}/${archive}`, archivePath);
    if (actual !== matches[0][1].toLowerCase()) {
      throw new Error(`SHA-256 checksum mismatch for ${archive}; the download will not be installed.`);
    }
    const extracted = path.join(temporary, 'extracted');
    if (os === 'windows') {
      await require('extract-zip')(archivePath, { dir: extracted });
    } else {
      const { mkdir } = require('node:fs/promises');
      await mkdir(extracted);
      await require('tar').x({ file: archivePath, cwd: extracted, strict: true });
    }
    const source = path.join(extracted, binary);
    if (!(await lstat(source)).isFile()) {
      throw new Error(`Release archive does not contain a regular ${binary} file.`);
    }
    await chmod(source, 0o755);
    await rename(source, path.join(__dirname, binary));
  } finally {
    await rm(temporary, { recursive: true, force: true });
  }
}

install().catch(error => {
  console.error(`vinted-cli installation failed: ${error.message}`);
  process.exitCode = 1;
});
