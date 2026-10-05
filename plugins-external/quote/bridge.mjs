// bridge.mjs — PaperValet quote plugin ↔ LyoSU/quote-api (vendored) bridge.
//
// stdin : JSON { messages, type, format, scale, backgroundColor, emojiBrand, assetsDir }
// stdout: binary frame — 4B magic "PVQT" + 4B payload length (BE) + 4B ext
//         code (1=png, 2=webp, 3=webm) + 4B reserved + payload bytes.
//
// First run downloads the official quote-api vendor code + assets + CJK
// fonts from GitHub raw into assetsDir (same URLs the TeleBox quote plugin
// uses), npm-installs the runtime deps, then calls generateQuote.
//
// All logs go to stderr so stdout stays a clean binary channel.

import fs from "node:fs";
import path from "node:path";
import util from "node:util";
import { execFileSync } from "node:child_process";
import { createRequire } from "node:module";

const MAGIC = Buffer.from("PVQT");
const EXT_CODES = { png: 1, webp: 2, webm: 3 };

const VENDOR_BASE = "https://raw.githubusercontent.com/TeleBoxOrg/TeleBox-Next-Plugins/main/quote";
const ASSETS_BASE = "https://raw.githubusercontent.com/LyoSU/quote-api/master/assets";
const VENDOR_FILES = [
  "generate.js",
  "vendor/emoji-db.js",
  "vendor/emoji-image.js",
  "vendor/image-load-path.js",
  "vendor/image-load-url.js",
  "vendor/index.js",
  "vendor/promise-concurrent.js",
  "vendor/quote-generate/attachments.js",
  "vendor/quote-generate/avatar.js",
  "vendor/quote-generate/canvas-utils.js",
  "vendor/quote-generate/color.js",
  "vendor/quote-generate/composer.js",
  "vendor/quote-generate/constants.js",
  "vendor/quote-generate/index.js",
  "vendor/quote-generate/layout-box.js",
  "vendor/quote-generate/media.js",
  "vendor/quote-generate/text-layout.js",
  "vendor/quote-generate/text-prepare.js",
  "vendor/quote-generate/text-render.js",
  "vendor/quote-generate/text-renderer.js",
  "vendor/user-name.js",
  "assets/icons/insert_drive_file.svg",
  "assets/icons/music_note.svg",
  "assets/icons/play_arrow.svg",
];
const ASSET_FILES = [
  "pattern_02.png",
  "pattern_ny.png",
  "emoji/emoji-apple-image.json",
  "emoji/emoji-google-image.json",
  "emoji/emoji-twitter-image.json",
  "emoji/emoji-joypixels-image.json",
  "emoji/emoji-blob-image.json",
];
const FONT_FILES = [
  { name: "NotoSansCJK-Regular.ttc", url: "https://github.com/notofonts/noto-cjk/raw/main/Sans/OTC/NotoSansCJK-Regular.ttc" },
  { name: "NotoSansCJK-Bold.ttc", url: "https://github.com/notofonts/noto-cjk/raw/main/Sans/OTC/NotoSansCJK-Bold.ttc" },
];
const NPM_DEPS = ["canvas", "sharp", "telegraf", "lru-cache@7", "runes", "jimp", "smartcrop-sharp", "emoji-db"];

// All logs go to stderr so stdout stays a clean binary channel — including
// console.log calls deep inside the vendor code ("Fonts loaded" etc).
console.log = (...args) => process.stderr.write(args.join(" ") + "\n");
console.info = console.log;
console.dir = (obj) => process.stderr.write(util.inspect(obj) + "\n");

function log(...args) { process.stderr.write(args.join(" ") + "\n"); }

function readStdinJson() {
  const raw = fs.readFileSync(0); // fd 0 = stdin
  return JSON.parse(raw.toString("utf8"));
}

async function fetchToBuffer(url) {
  const res = await fetch(url, { redirect: "follow" });
  if (!res.ok) throw new Error(`GET ${url} failed: ${res.status} ${res.statusText}`);
  return Buffer.from(await res.arrayBuffer());
}

async function downloadIfMissing(url, filePath) {
  fs.mkdirSync(path.dirname(filePath), { recursive: true });
  const data = await fetchToBuffer(url);
  fs.writeFileSync(filePath, data);
}

async function ensureAssets(dir) {
  const vendorDir = path.join(dir, "vendor-src");
  const ready = path.join(dir, ".ready");
  if (fs.existsSync(ready)) return vendorDir;
  log(`[quote-bridge] first run: downloading quote-api vendor + assets (${VENDOR_FILES.length + ASSET_FILES.length + FONT_FILES.length} files)…`);
  const jobs = [];
  for (const rel of VENDOR_FILES) jobs.push(downloadIfMissing(`${VENDOR_BASE}/${rel}`, path.join(vendorDir, rel)));
  for (const rel of ASSET_FILES) jobs.push(downloadIfMissing(`${ASSETS_BASE}/${rel}`, path.join(dir, "assets", rel)));
  for (const f of FONT_FILES) jobs.push(downloadIfMissing(f.url, path.join(dir, "assets", f.name)));
  await Promise.all(jobs);
  // generate.js resolves the pattern asset from process.cwd()/assets/quote —
  // the bridge runs with cwd = data dir, so point assets/quote → assets/.
  const assetsQuote = path.join(dir, "assets", "quote");
  try { fs.unlinkSync(assetsQuote); } catch { /* absent */ }
  fs.symlinkSync(path.join(dir, "assets"), assetsQuote, "dir");
  // vendor emoji json resolves relative to vendor/../assets/emoji — link it.
  const vendorEmoji = path.join(vendorDir, "assets", "emoji");
  try { fs.rmSync(vendorEmoji, { recursive: true, force: true }); } catch { /* absent */ }
  fs.mkdirSync(path.dirname(vendorEmoji), { recursive: true });
  fs.symlinkSync(path.join(dir, "assets", "emoji"), vendorEmoji, "dir");
  fs.writeFileSync(ready, new Date().toISOString());
  log("[quote-bridge] assets ready");
  return vendorDir;
}

function ensureNpmDeps(dir) {
  const nm = path.join(dir, "node_modules");
  const missing = NPM_DEPS.filter((dep) => !fs.existsSync(path.join(nm, dep)));
  if (missing.length === 0) return;
  log(`[quote-bridge] npm install ${missing.join(" ")} …`);
  const pkgPath = path.join(dir, "package.json");
  if (!fs.existsSync(pkgPath)) fs.writeFileSync(pkgPath, JSON.stringify({ name: "papervalet-quote-runtime", private: true }, null, 2));
  execFileSync("npm", ["install", "--no-fund", "--no-audit", "--loglevel=error", ...missing], {
    cwd: dir,
    stdio: ["ignore", "pipe", "pipe"],
    encoding: "utf8",
  });
  log("[quote-bridge] npm install done");
}

function frame(payload, ext) {
  const code = EXT_CODES[ext] || EXT_CODES.png;
  const hdr = Buffer.alloc(16);
  MAGIC.copy(hdr, 0);
  hdr.writeUInt32BE(payload.length, 4);
  hdr.writeUInt32BE(code, 8);
  return Buffer.concat([hdr, payload]);
}

function decodeBase64Images(messages) {
  for (const m of messages) {
    if (!m) continue;
    if (m.avatarBuffer && typeof m.avatarBuffer === "string") {
      try { m.avatarBuffer = Buffer.from(m.avatarBuffer, "base64"); } catch { delete m.avatarBuffer; m.avatar = false; }
    }
    if (m.mediaCanvas && typeof m.mediaCanvas === "string") {
      try { m.mediaCanvas = Buffer.from(m.mediaCanvas, "base64"); } catch { delete m.mediaCanvas; }
    }
    if (m.replyMessage && m.replyMessage.avatarBuffer && typeof m.replyMessage.avatarBuffer === "string") {
      try { m.replyMessage.avatarBuffer = Buffer.from(m.replyMessage.avatarBuffer, "base64"); } catch { delete m.replyMessage.avatarBuffer; }
    }
  }
}

async function main() {
  const req = readStdinJson();
  const dir = req.assetsDir || process.cwd();
  fs.mkdirSync(dir, { recursive: true });
  // The vendor resolves pattern/fonts/icons from process.cwd()/assets[/quote]
  // (same layout TeleBox uses). chdir so this holds however node was spawned.
  process.chdir(dir);
  const vendorDir = await ensureAssets(dir);
  ensureNpmDeps(dir);

  decodeBase64Images(req.messages || []);

  const vendorRequire = createRequire(path.join(vendorDir, "generate.js"));
  const { generateQuote } = vendorRequire("./generate.js");

  const result = await generateQuote({
    messages: req.messages,
    type: req.type || "quote",
    format: req.format || "webp",
    scale: req.scale || 2,
    backgroundColor: req.backgroundColor,
    emojiBrand: req.emojiBrand || "apple",
  });

  if (!result || result.error) {
    throw new Error(`generateQuote failed: ${(result && result.error) || "no result"}`);
  }
  if (!result.image || !result.image.length) {
    throw new Error("generateQuote returned an empty image");
  }

  const ext = EXT_CODES[result.ext] ? result.ext : (result.ext === "webm" ? "webm" : "png");
  process.stdout.write(frame(result.image, ext));
  log(`[quote-bridge] ok: ${result.image.length} bytes ${result.ext} ${result.width}x${result.height}`);
}

main().catch((err) => {
  process.stderr.write(`[quote-bridge] FATAL ${err && err.stack ? err.stack : err}\n`);
  process.exit(1);
});
