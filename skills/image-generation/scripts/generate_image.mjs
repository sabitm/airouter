#!/usr/bin/env node
import fs from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import path from 'node:path';
import { parseArgs, TextDecoder } from 'node:util';

const MAX_INPUT_BYTES = 1 << 20;
const MAX_RESPONSE_BYTES = 64 << 20;
const MAX_IMAGE_BYTES = 48 << 20;
const MAX_JOBS = 64;
const QUALITIES = ['auto', 'low', 'medium', 'high'];
const BACKGROUNDS = ['auto', 'opaque'];
const DETAIL_COUNTS = ['text_tokens', 'image_tokens', 'cached_tokens', 'cache_write_tokens', 'reasoning_tokens'];

class InputError extends Error {}
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
    console.log(`Generate PNG files through Airouter without third-party packages.
Usage: node generate_image.mjs --prompt TEXT --output FILE.png [options]
       node generate_image.mjs --prompt-file FILE --output FILE.png [options]
       node generate_image.mjs --batch FILE.json [options]
Options:
  --url URL               Base URL (AIROUTER_URL or http://localhost:31415)
  --model COMBO           Combo name (AIROUTER_IMAGE_MODEL or image)
  --quality VALUE         auto, low, medium, high (default auto)
  --background VALUE      auto, opaque (default auto)
  --timeout SECONDS       Network operation timeout, 0 < value <= 3600 (default 330)
  --help                  Show this help
Authentication: optional AIROUTER_API_KEY environment variable.
Batch: JSON array of 1-64 prompt/output jobs; quality/background may override defaults.
Outputs are PNG files; existing files are never overwritten. Requests run serially.`);
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

function endpoint(value) {
  try {
    if (typeof value !== 'string' || !/^https?:\/\//iu.test(value) || /[\s\x00-\x1f\x7f]/u.test(value) || value.includes('\\')) throw new Error();
    const url = new URL(value);
    if (!['http:', 'https:'].includes(url.protocol) || !url.hostname || url.username || url.password
        || url.search || url.hash || url.port === '0') throw new Error();
    const root = url.pathname.replace(/\/+$/u, '');
    url.pathname = root + (root.endsWith('/v1') ? '/images/generations' : '/v1/images/generations');
    return url;
  } catch {
    throw new InputError('url must be an HTTP(S) base URL without credentials, whitespace, query, or fragment');
  }
}

function textWithinLimit(value, max) {
  return typeof value === 'string' && value.trim() !== '' && value.isWellFormed()
    && Buffer.byteLength(value, 'utf8') <= max;
}

function loadJobs(args) {
  if (!Number.isFinite(args.timeout) || args.timeout <= 0 || args.timeout > 3600) {
    throw new InputError('timeout must be greater than 0 and at most 3600 seconds');
  }
  if (!textWithinLimit(args.model, 1024)) {
    throw new InputError('model must be a nonempty combo name of at most 1024 UTF-8 bytes');
  }
  let jobs;
  if (args.batch !== undefined) {
    if (args.output !== undefined) throw new InputError('output is not allowed with batch');
    const text = readInput(args.batch);
    try { jobs = JSON.parse(text); } catch { throw new InputError('batch must be valid JSON'); }
    if (!Array.isArray(jobs) || jobs.length < 1 || jobs.length > MAX_JOBS) {
      throw new InputError('batch must contain 1 to 64 jobs');
    }
  } else {
    jobs = [{ prompt: args['prompt-file'] !== undefined ? readInput(args['prompt-file']) : args.prompt, output: args.output }];
  }
  return jobs.map(job => {
    if (!job || typeof job !== 'object' || Array.isArray(job)
        || Object.keys(job).some(key => !['prompt', 'output', 'quality', 'background'].includes(key))) {
      throw new InputError('each job allows only prompt, output, quality, and background');
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
    return { prompt: job.prompt, output: job.output, quality, background };
  });
}

function reserveOutputs(jobs) {
  const outputs = [];
  const seen = new Set();
  try {
    for (const job of jobs) {
      let filename = path.resolve(job.output);
      fs.mkdirSync(path.dirname(filename), { recursive: true });
      filename = path.join(fs.realpathSync(path.dirname(filename)), path.basename(filename));
      const key = process.platform === 'win32' ? filename.toLowerCase() : filename;
      if (seen.has(key)) throw new InputError('output paths must be unique and must not already exist');
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

const crcTable = new Uint32Array(256);
for (let n = 0; n < 256; n++) {
  let crc = n;
  for (let bit = 0; bit < 8; bit++) crc = (crc & 1) ? 0xedb88320 ^ (crc >>> 1) : crc >>> 1;
  crcTable[n] = crc >>> 0;
}

function pngDimensions(image) {
  if (!image.subarray(0, 8).equals(Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]))) {
    throw new JobError('invalid_response');
  }
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
for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    interrupted = true;
    activeRequest?.destroy(new JobError('interrupted'));
  });
}

function generate(url, model, job, key, timeout) {
  return new Promise((resolve, reject) => {
    const payload = Buffer.from(JSON.stringify({
      model, prompt: job.prompt, n: 1, size: 'auto', quality: job.quality,
      background: job.background, output_format: 'png', response_format: 'b64_json', stream: false,
    }));
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
    const url = endpoint(args.url);
    const jobs = loadJobs(args);
    const key = process.env.AIROUTER_API_KEY || '';
    if (/[^\x21-\x7e]/u.test(key)) throw new InputError('AIROUTER_API_KEY must contain only visible ASCII characters');
    outputs = reserveOutputs(jobs);
    let failed = false;
    for (let i = 0; i < jobs.length; i++) {
      const output = outputs[i];
      const result = { index: i + 1, ok: false, path: output.path };
      try {
        if (interrupted) throw new JobError('interrupted');
        const { image, metadata } = await generate(url, args.model, jobs[i], key, args.timeout);
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
    const message = error instanceof InputError ? error.message : 'invalid input or inaccessible output path; no generation request was sent';
    console.error(`error: ${message}`);
    return 2;
  } finally {
    cleanup(outputs);
  }
}

if (Number(process.versions.node.split('.')[0]) < 22) {
  console.error('Node.js 22 or newer is required');
  process.exitCode = 2;
} else {
  process.exitCode = await main();
}
