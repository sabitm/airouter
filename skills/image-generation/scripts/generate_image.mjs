#!/usr/bin/env node
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import path from 'node:path';
import { setImmediate } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import { parseArgs, TextDecoder } from 'node:util';

const MAX_INPUT_BYTES = 1 << 20;
const MAX_RESPONSE_BYTES = 64 << 20;
const MAX_IMAGE_BYTES = 48 << 20;
export const MAX_REQUEST_BYTES = 64 << 20;
export const MAX_REFERENCE_BYTES = 10 << 20;
export const MAX_REFERENCE_TOTAL_BYTES = 32 << 20;
const MAX_REFERENCES = 5;
const MAX_JOBS = 64;
const QUALITIES = ['auto', 'low', 'medium', 'high'];
const BACKGROUNDS = ['auto', 'opaque'];
const DETAIL_COUNTS = ['text_tokens', 'image_tokens', 'cached_tokens', 'cache_write_tokens', 'reasoning_tokens'];
const PNG_SIGNATURE = Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]);

export function requestExceedsLimit(payload) {
  return payload.length > MAX_REQUEST_BYTES;
}

class InputError extends Error {}
class InterruptError extends Error {}
class JobError extends Error {
  constructor(code, status) {
    super(code);
    this.code = code;
    this.status = status;
  }
}

function argumentsForRun() {
  let values;
  try {
    ({ values } = parseArgs({
      options: {
        prompt: { type: 'string' }, 'prompt-file': { type: 'string' },
        batch: { type: 'string' }, output: { type: 'string' },
        image: { type: 'string', multiple: true },
        url: { type: 'string', default: process.env.AIROUTER_URL || 'http://localhost:31415' },
        model: { type: 'string', default: process.env.AIROUTER_IMAGE_MODEL || 'image' },
        quality: { type: 'string', default: 'auto' },
        background: { type: 'string', default: 'auto' },
        timeout: { type: 'string', default: '330' }, help: { type: 'boolean' },
      },
    }));
  } catch {
    throw new InputError('invalid arguments; run --help');
  }
  if (values.help) {
    console.log(`Generate or edit PNG files through Airouter without third-party packages.
Usage: node generate_image.mjs --prompt TEXT --output FILE.png [options]
       node generate_image.mjs --prompt-file FILE --output FILE.png [options]
       node generate_image.mjs --batch FILE.json [options]
Options:
  --image PATH            Local reference image; repeat up to 5 times. Not allowed with --batch
  --url URL               Base URL (AIROUTER_URL or http://localhost:31415)
  --model COMBO           Combo name (AIROUTER_IMAGE_MODEL or image)
  --quality VALUE         auto, low, medium, high (default auto)
  --background VALUE      auto, opaque (default auto)
  --timeout SECONDS       Network operation timeout, 0 < value <= 3600 (default 330)
  --help                  Show this help
Authentication: optional AIROUTER_API_KEY environment variable.
Batch: JSON array of 1-64 prompt/output jobs; quality/background may override defaults.
Optional images arrays select edits. Outputs are PNG files and are never overwritten.
Requests run serially. No references selects generation.`);
    return null;
  }
  if (['prompt', 'prompt-file', 'batch'].filter(key => values[key] !== undefined).length !== 1
      || !QUALITIES.includes(values.quality) || !BACKGROUNDS.includes(values.background)) {
    throw new InputError('invalid arguments; run --help');
  }
  values.timeout = Number(values.timeout);
  return values;
}

function readInput(filename) {
  let fd;
  try {
    fd = fs.openSync(filename, 'r');
    const buffer = Buffer.alloc(MAX_INPUT_BYTES + 1);
    let length = 0;
    while (length < buffer.length) {
      const count = fs.readSync(fd, buffer, length, buffer.length - length, null);
      if (count === 0) break;
      length += count;
    }
    if (length > MAX_INPUT_BYTES) throw new InputError('input file exceeds 1 MiB');
    return new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(buffer.subarray(0, length));
  } catch (error) {
    if (error instanceof InputError) throw error;
    throw new InputError('cannot read UTF-8 input file');
  } finally {
    if (fd !== undefined) fs.closeSync(fd);
  }
}

function endpointBuilder(value) {
  try {
    if (typeof value !== 'string' || !/^https?:\/\//iu.test(value) || /[\s\x00-\x1f\x7f]/u.test(value) || value.includes('\\')) throw new Error();
    const parsed = new URL(value);
    if (!['http:', 'https:'].includes(parsed.protocol) || !parsed.hostname || parsed.username || parsed.password
        || parsed.search || parsed.hash || parsed.port === '0') throw new Error();
    const root = parsed.pathname.replace(/\/+$/u, '');
    return edit => {
      const url = new URL(parsed.href);
      const suffix = edit ? '/images/edits' : '/images/generations';
      url.pathname = root + (root.endsWith('/v1') ? suffix : '/v1' + suffix);
      url.search = '';
      url.hash = '';
      return url;
    };
  } catch {
    throw new InputError('url must be an HTTP(S) base URL without credentials, whitespace, query, or fragment');
  }
}

function textWithinLimit(value, max) {
  return typeof value === 'string' && value.trim() !== '' && value.isWellFormed()
    && Buffer.byteLength(value, 'utf8') <= max;
}

function referencePaths(value) {
  if (!Array.isArray(value) || value.length < 1 || value.length > MAX_REFERENCES
      || value.some(item => typeof item !== 'string' || !item.trim() || !item.isWellFormed() || item.includes('\0'))) {
    throw new InputError('each images list must contain 1 to 5 paths');
  }
  return value;
}

function loadJobs(args) {
  if (!Number.isFinite(args.timeout) || args.timeout <= 0 || args.timeout > 3600) {
    throw new InputError('timeout must be greater than 0 and at most 3600 seconds');
  }
  if (!textWithinLimit(args.model, 1024)) {
    throw new InputError('model must be a nonempty combo name of at most 1024 UTF-8 bytes');
  }
  if (args.batch !== undefined && args.image !== undefined) throw new InputError('--image is not allowed with batch');
  let jobs;
  if (args.batch !== undefined) {
    if (args.output !== undefined) throw new InputError('output is not allowed with batch');
    const text = readInput(args.batch);
    try { jobs = JSON.parse(text); } catch { throw new InputError('batch must be valid JSON'); }
    if (!Array.isArray(jobs) || jobs.length < 1 || jobs.length > MAX_JOBS) {
      throw new InputError('batch must contain 1 to 64 jobs');
    }
  } else {
    const images = args.image || [];
    if (images.length > MAX_REFERENCES) throw new InputError('at most 5 reference images are allowed');
    const single = {
      prompt: args['prompt-file'] !== undefined ? readInput(args['prompt-file']) : args.prompt,
      output: args.output,
    };
    if (images.length) single.images = images;
    jobs = [single];
  }
  return jobs.map(job => {
    if (!job || typeof job !== 'object' || Array.isArray(job)
        || Object.keys(job).some(key => !['prompt', 'output', 'quality', 'background', 'images'].includes(key))) {
      throw new InputError('each job allows only prompt, output, quality, background, and images');
    }
    if (!textWithinLimit(job.prompt, MAX_INPUT_BYTES)) {
      throw new InputError('each prompt must be nonempty and at most 1 MiB of UTF-8');
    }
    if (typeof job.output !== 'string' || !job.output.trim() || !job.output.isWellFormed()
        || job.output.includes('\0') || path.extname(job.output).toLowerCase() !== '.png') {
      throw new InputError('each output must be a nonempty PNG path');
    }
    const quality = Object.hasOwn(job, 'quality') ? job.quality : args.quality;
    const background = Object.hasOwn(job, 'background') ? job.background : args.background;
    if (!QUALITIES.includes(quality) || !BACKGROUNDS.includes(background)) {
      throw new InputError('unsupported quality or background');
    }
    const images = Object.hasOwn(job, 'images') ? referencePaths(job.images) : [];
    return { prompt: job.prompt, output: job.output, quality, background, images, references: [] };
  });
}

function pathKey(filename) {
  return process.platform === 'win32' ? filename.toLowerCase() : filename;
}

function fileIdentity(stats) {
  if (!stats || !stats.ino) return null;
  return `${stats.dev}:${stats.ino}`;
}

function locateReference(value) {
  if (typeof value !== 'string' || !value.trim() || !value.isWellFormed() || value.includes('\0')) {
    throw new InputError('each reference must be a local file path');
  }
  const absolute = path.resolve(value);
  let listed;
  try { listed = fs.lstatSync(absolute); } catch {
    throw new InputError('reference image is missing or is not a regular file');
  }
  if (listed.isSymbolicLink() || !listed.isFile()) {
    throw new InputError('reference image is missing or is not a regular file');
  }
  let parent;
  try { parent = fs.realpathSync(path.dirname(absolute)); } catch {
    throw new InputError('reference image is missing or is not a regular file');
  }
  return path.join(parent, path.basename(absolute));
}

function openNoFollow(filename) {
  let flags = fs.constants.O_RDONLY;
  if (fs.constants.O_NOFOLLOW) flags |= fs.constants.O_NOFOLLOW;
  if (fs.constants.O_NONBLOCK) flags |= fs.constants.O_NONBLOCK;
  return fs.openSync(filename, flags);
}

function referenceMime(header) {
  // Signature check only. This is not pixel decoding or a filesystem sandbox.
  if (header.length >= 8 && header.subarray(0, 8).equals(PNG_SIGNATURE)) return 'image/png';
  if (header.length >= 3 && header[0] === 0xff && header[1] === 0xd8 && header[2] === 0xff) return 'image/jpeg';
  if (header.length >= 12 && header.subarray(0, 4).toString('latin1') === 'RIFF' && header.subarray(8, 12).toString('latin1') === 'WEBP') {
    return 'image/webp';
  }
  return null;
}

async function preflightReference(value, remaining) {
  const filename = locateReference(value);
  let fd;
  try {
    fd = openNoFollow(filename);
    const opened = fs.fstatSync(fd);
    const listed = fs.lstatSync(filename);
    if (!opened.isFile() || !listed.isFile() || fileIdentity(opened) !== fileIdentity(listed)) {
      throw new InputError('reference image is missing or is not a regular file');
    }
    if (opened.size <= 0) throw new InputError('unsupported reference image');
    if (opened.size > MAX_REFERENCE_BYTES) throw new InputError('reference exceeds size limit');
    if (opened.size > remaining) throw new InputError('references exceed total size budget');
    const hash = crypto.createHash('sha256');
    const header = Buffer.alloc(12);
    let headerLen = 0;
    let total = 0;
    const chunk = Buffer.alloc(64 * 1024);
    while (true) {
      // Signal callbacks cannot run during synchronous reads, even with small buffers.
      await setImmediate();
      if (interrupted) throw new InterruptError();
      const count = fs.readSync(fd, chunk, 0, chunk.length, null);
      if (count === 0) break;
      total += count;
      if (total > MAX_REFERENCE_BYTES) throw new InputError('reference exceeds size limit');
      if (headerLen < 12) {
        const take = Math.min(12 - headerLen, count);
        chunk.copy(header, headerLen, 0, take);
        headerLen += take;
      }
      hash.update(chunk.subarray(0, count));
    }
    if (total !== opened.size) throw new InputError('reference image changed during validation');
    const mime = referenceMime(header.subarray(0, headerLen));
    if (mime === null) throw new InputError('unsupported reference image');
    return { path: filename, id: fileIdentity(opened), size: total, sha256: hash.digest(), mime };
  } catch (error) {
    if (error instanceof InputError || error instanceof InterruptError) throw error;
    if (interrupted) throw new InterruptError();
    throw new InputError('reference image is missing or is not a regular file');
  } finally {
    if (fd !== undefined) fs.closeSync(fd);
  }
}

async function prepareReferences(jobs) {
  for (const job of jobs) {
    const metas = [];
    const keys = new Set();
    const identities = new Set();
    let total = 0;
    for (const rawPath of job.images) {
      const meta = await preflightReference(rawPath, MAX_REFERENCE_TOTAL_BYTES - total);
      const key = pathKey(meta.path);
      if (keys.has(key) || (meta.id !== null && identities.has(meta.id))) {
        throw new InputError('duplicate reference in a job');
      }
      keys.add(key);
      if (meta.id !== null) identities.add(meta.id);
      total += meta.size;
      metas.push(meta);
    }
    job.references = metas;
  }
}

function reserveOutputs(jobs) {
  const outputs = [];
  const seen = new Set();
  const refKeys = new Set();
  for (const job of jobs) {
    for (const ref of job.references) refKeys.add(pathKey(ref.path));
  }
  try {
    for (const job of jobs) {
      let filename = path.resolve(job.output);
      fs.mkdirSync(path.dirname(filename), { recursive: true });
      filename = path.join(fs.realpathSync(path.dirname(filename)), path.basename(filename));
      const key = pathKey(filename);
      if (seen.has(key) || refKeys.has(key)) throw new InputError('output paths must be unique and must not already exist');
      seen.add(key);
      let fd;
      try { fd = fs.openSync(filename, 'wx', 0o600); } catch (error) {
        if (error.code === 'EEXIST') throw new InputError('output paths must be unique and must not already exist');
        throw error;
      }
      outputs.push({ path: filename, fd, saved: false });
    }
    return outputs;
  } catch (error) {
    cleanup(outputs);
    throw error;
  }
}

function cleanup(outputs) {
  for (const output of outputs) {
    if (output.fd !== null) {
      fs.closeSync(output.fd);
      output.fd = null;
    }
    if (!output.saved) {
      try { fs.unlinkSync(output.path); } catch (error) {
        if (error.code !== 'ENOENT') throw error;
      }
    }
  }
}

async function readVerified(meta) {
  if (interrupted) throw new JobError('interrupted');
  let fd;
  try {
    fd = openNoFollow(meta.path);
    const opened = fs.fstatSync(fd);
    const listed = fs.lstatSync(meta.path);
    if (!opened.isFile() || !listed.isFile() || fileIdentity(opened) !== meta.id
        || fileIdentity(opened) !== fileIdentity(listed) || opened.size !== meta.size || listed.size !== meta.size) {
      throw new JobError('reference_changed');
    }
    const raw = Buffer.alloc(meta.size);
    let offset = 0;
    while (offset < meta.size) {
      await setImmediate();
      if (interrupted) throw new JobError('interrupted');
      const count = fs.readSync(fd, raw, offset, Math.min(64 * 1024, meta.size - offset), null);
      if (count === 0) break;
      offset += count;
    }
    await setImmediate();
    if (interrupted) throw new JobError('interrupted');
    const extra = fs.readSync(fd, Buffer.alloc(1), 0, 1, null);
    const after = fs.fstatSync(fd);
    const current = fs.lstatSync(meta.path);
    // A matching prefix is not the whole reference if the file grew during the read.
    if (offset !== meta.size || extra !== 0 || !after.isFile() || !current.isFile()
        || fileIdentity(after) !== meta.id || fileIdentity(current) !== meta.id
        || after.size !== meta.size || current.size !== meta.size
        || !crypto.createHash('sha256').update(raw).digest().equals(meta.sha256)) {
      throw new JobError('reference_changed');
    }
    return raw;
  } catch (error) {
    if (error instanceof JobError) throw error;
    if (interrupted) throw new JobError('interrupted');
    throw new JobError('reference_changed');
  } finally {
    if (fd !== undefined) fs.closeSync(fd);
  }
}

async function requestPayload(model, job) {
  let body;
  if (job.references.length) {
    const images = [];
    for (const meta of job.references) {
      const raw = await readVerified(meta);
      images.push({ image_url: `data:${meta.mime};base64,${raw.toString('base64')}` });
      raw.fill(0);
    }
    body = {
      model, prompt: job.prompt, images, n: 1, size: 'auto', quality: job.quality, background: job.background,
    };
  } else {
    body = {
      model, prompt: job.prompt, n: 1, size: 'auto', quality: job.quality, background: job.background,
      output_format: 'png', response_format: 'b64_json', stream: false,
    };
  }
  const payload = Buffer.from(JSON.stringify(body));
  if (requestExceedsLimit(payload)) throw new JobError('request_too_large');
  return payload;
}

const crcTable = new Uint32Array(256);
for (let n = 0; n < 256; n++) {
  let crc = n;
  for (let bit = 0; bit < 8; bit++) crc = (crc & 1) ? 0xedb88320 ^ (crc >>> 1) : crc >>> 1;
  crcTable[n] = crc >>> 0;
}

function pngDimensions(image) {
  if (!image.subarray(0, 8).equals(PNG_SIGNATURE)) throw new JobError('invalid_response');
  let position = 8, chunks = 0, width = 0, height = 0, hasData = false;
  while (position + 12 <= image.length) {
    const length = image.readUInt32BE(position);
    const end = position + length + 12;
    if (end > image.length) break;
    const kind = image.toString('latin1', position + 4, position + 8);
    let crc = 0xffffffff;
    for (let i = position + 4; i < end - 4; i++) crc = crcTable[(crc ^ image[i]) & 255] ^ (crc >>> 8);
    if (((crc ^ 0xffffffff) >>> 0) !== image.readUInt32BE(end - 4)) break;
    if (chunks === 0) {
      if (kind !== 'IHDR' || length !== 13) break;
      width = image.readUInt32BE(position + 8);
      height = image.readUInt32BE(position + 12);
      if (!width || !height) break;
    } else if (kind === 'IHDR') break;
    hasData ||= kind === 'IDAT' && length > 0;
    if (kind === 'IEND') {
      if (length === 0 && hasData && end === image.length) return [width, height];
      break;
    }
    chunks++;
    position = end;
  }
  throw new JobError('invalid_response');
}

function validUsage(value) {
  const count = n => Number.isSafeInteger(n) && n >= 0;
  if (!value || typeof value !== 'object' || Array.isArray(value)
      || !count(value.input_tokens) || !count(value.output_tokens)) return null;
  const out = { input_tokens: value.input_tokens, output_tokens: value.output_tokens };
  if (Object.hasOwn(value, 'total_tokens')) {
    if (!count(value.total_tokens)) return null;
    out.total_tokens = value.total_tokens;
  }
  for (const key of ['input_tokens_details', 'output_tokens_details']) {
    if (value[key] != null) {
      const details = value[key];
      if (typeof details !== 'object' || Array.isArray(details) || !Object.values(details).every(count)) return null;
      out[key] = Object.fromEntries(Object.entries(details).filter(([name]) => DETAIL_COUNTS.includes(name)));
    }
  }
  return out;
}

function decodeResponse(raw) {
  try {
    const response = JSON.parse(new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(raw));
    if (!response || typeof response !== 'object' || Array.isArray(response)
        || (Object.hasOwn(response, 'output_format') && response.output_format !== 'png')) throw new Error();
    const items = response.data;
    if (!Array.isArray(items) || items.length !== 1 || !items[0] || typeof items[0] !== 'object') throw new Error();
    const encoded = items[0].b64_json;
    if (typeof encoded !== 'string' || !encoded || encoded.length > MAX_RESPONSE_BYTES
        || encoded.length % 4 || !/^[A-Za-z0-9+/]*={0,2}$/u.test(encoded)) throw new Error();
    // Buffer.from tolerates malformed base64; require a canonical round trip as well.
    const image = Buffer.from(encoded, 'base64');
    if (image.length > MAX_IMAGE_BYTES || image.toString('base64') !== encoded) throw new Error();
    const [width, height] = pngDimensions(image);
    const metadata = { bytes: image.length, size: `${width}x${height}`, output_format: 'png' };
    for (const [key, choices] of [['quality', QUALITIES], ['background', BACKGROUNDS]]) {
      if (choices.includes(response[key])) metadata[key] = response[key];
    }
    if (Number.isSafeInteger(response.created) && response.created >= 0) metadata.created = response.created;
    const id = items[0].generation_id;
    if (typeof id === 'string' && /^[A-Za-z0-9_-]{1,256}$/u.test(id)) metadata.generation_id = id;
    const usage = validUsage(response.usage);
    if (usage !== null) metadata.usage = usage;
    return { image, metadata };
  } catch {
    throw new JobError('invalid_response');
  }
}

let interrupted = false;
let activeRequest;

async function generate(url, model, job, key, timeout) {
  const payload = await requestPayload(model, job);
  await setImmediate();
  if (interrupted) throw new JobError('interrupted');
  return new Promise((resolve, reject) => {
    const headers = { 'Content-Type': 'application/json', Accept: 'application/json', 'Content-Length': payload.length };
    if (key) headers.Authorization = `Bearer ${key}`;
    const transport = url.protocol === 'https:' ? https : http;
    let failure;
    // Built-in HTTP clients do not follow redirects; disable environment proxy support explicitly.
    const request = transport.request(url, { method: 'POST', headers, agent: false, proxyEnv: {} }, response => {
      const fail = error => { reject(error); response.destroy(); };
      if (response.statusCode < 200 || response.statusCode >= 300) {
        fail(new JobError('http_error', response.statusCode));
        return;
      }
      const declared = response.headers['content-length'];
      if (declared && Number(declared) > MAX_RESPONSE_BYTES) {
        fail(new JobError('response_too_large'));
        return;
      }
      const chunks = [];
      let length = 0;
      response.on('data', chunk => {
        length += chunk.length;
        if (length > MAX_RESPONSE_BYTES) fail(new JobError('response_too_large'));
        else chunks.push(chunk);
      });
      response.on('error', () => reject(interrupted ? new JobError('interrupted') : failure || new JobError('network_error')));
      response.on('end', () => {
        try { resolve(decodeResponse(Buffer.concat(chunks, length))); } catch (error) { reject(error); }
      });
    });
    activeRequest = request;
    request.setTimeout(timeout * 1000, () => {
      failure = new JobError('timeout');
      request.destroy(failure);
    });
    request.on('error', error => reject(error instanceof JobError ? error : new JobError('network_error')));
    request.end(payload);
  });
}

async function main() {
  let outputs = [];
  try {
    const args = argumentsForRun();
    if (args === null) return 0;
    const urls = endpointBuilder(args.url);
    const jobs = loadJobs(args);
    const key = process.env.AIROUTER_API_KEY || '';
    if (/[^\x21-\x7e]/u.test(key)) throw new InputError('AIROUTER_API_KEY must contain only visible ASCII characters');
    await prepareReferences(jobs);
    await setImmediate();
    if (interrupted) throw new InterruptError();
    outputs = reserveOutputs(jobs);
    let failed = false;
    for (let i = 0; i < jobs.length; i++) {
      const output = outputs[i];
      const result = { index: i + 1, ok: false, path: output.path };
      try {
        if (interrupted) throw new JobError('interrupted');
        const { image, metadata } = await generate(urls(jobs[i].references.length > 0), args.model, jobs[i], key, args.timeout);
        activeRequest = undefined;
        fs.writeFileSync(output.fd, image);
        fs.closeSync(output.fd);
        output.fd = null;
        output.saved = true;
        Object.assign(result, { ok: true }, metadata);
      } catch (error) {
        result.error = error instanceof JobError ? error.code : 'output_error';
        if (error instanceof JobError && error.status !== undefined) result.http_status = error.status;
      } finally {
        activeRequest = undefined;
      }
      if (!output.saved) cleanup([output]);
      console.log(JSON.stringify(result));
      if (interrupted) return 130;
      failed ||= !result.ok;
    }
    return failed ? 1 : 0;
  } catch (error) {
    if (interrupted || error instanceof InterruptError) {
      console.error('error: interrupted');
      return 130;
    }
    const message = error instanceof InputError ? error.message : 'invalid input or inaccessible output path; no generation request was sent';
    console.error(`error: ${message}`);
    return 2;
  } finally {
    cleanup(outputs);
  }
}

const invokedDirectly = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (invokedDirectly) {
  for (const signal of ['SIGINT', 'SIGTERM']) {
    process.on(signal, () => {
      interrupted = true;
      activeRequest?.destroy(new JobError('interrupted'));
    });
  }
  if (Number(process.versions.node.split('.')[0]) < 22) {
    console.error('Node.js 22 or newer is required');
    process.exitCode = 2;
  } else {
    process.exitCode = await main();
  }
}
