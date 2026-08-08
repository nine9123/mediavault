#!/usr/bin/env node
// drive-people-page.js — run the People page's real JavaScript in Node and
// click through it, with /api/* answered from a script.
//
// Why this exists: internal/server/web/index.html is a single-file vanilla-JS
// app with no build step and no test runner, so the Go tests cover the API and
// nothing covers the page. This loads the actual <script> out of the actual file
// into a vm context behind a hand-rolled DOM just complete enough for it, then
// drives the merge flow the way a user would — click "Merge…", click "Merge
// here", check what got POSTed and what the cards say afterwards.
//
// Usage:
//   node .claude-task/tools/drive-people-page.js [path/to/index.html]
//
// Exits non-zero on the first failed check. Needs nothing but node.
//
// The DOM shim is a stub, not an implementation: no layout, no real event
// dispatch, no CSS. It is enough for code that builds elements and appends them,
// which is all this page does. If the page grows to use a DOM feature that isn't
// here, add it — a missing method shows up as "LOAD FAILED" rather than a
// silently wrong result.
const fs = require("fs");
const path = process.argv[2] || require("path").join(__dirname, "..", "..", "internal", "server", "web", "index.html");
const html = fs.readFileSync(path, "utf8");
const script = html.match(/<script[^>]*>([\s\S]*?)<\/script>/)[1];

function El(tag) {
  const e = {
    tagName: (tag || "div").toUpperCase(),
    children: [], handlers: {}, style: {}, dataset: {}, attrs: {},
    className: "", textContent: "", title: "", type: "", src: "", value: "",
    hidden: false, checked: false,
    classList: {
      add(...c) { c.forEach(x => { if (!e._cls().includes(x)) e.className = (e.className + " " + x).trim(); }); },
      remove(...c) { e.className = e._cls().filter(x => !c.includes(x)).join(" "); },
      contains(c) { return e._cls().includes(c); },
      toggle(c, on) { on === undefined ? (e.classList.contains(c) ? e.classList.remove(c) : e.classList.add(c)) : (on ? e.classList.add(c) : e.classList.remove(c)); },
    },
    _cls() { return e.className.split(/\s+/).filter(Boolean); },
    appendChild(c) { e.children.push(c); c.parent = e; return c; },
    insertBefore(c) { e.children.unshift(c); return c; },
    removeChild(c) { e.children = e.children.filter(x => x !== c); },
    remove() { if (e.parent) e.parent.removeChild(e); },
    replaceWith(o) { if (e.parent) { const i = e.parent.children.indexOf(e); if (i >= 0) e.parent.children[i] = o; } },
    addEventListener(k, fn) { (e.handlers[k] = e.handlers[k] || []).push(fn); },
    removeEventListener() {},
    setAttribute(k, v) { e.attrs[k] = v; }, removeAttribute(k) { delete e.attrs[k]; },
    getAttribute(k) { return e.attrs[k]; },
    querySelector() { return null; }, querySelectorAll() { return []; },
    focus() {}, blur() {}, scrollIntoView() {}, getBoundingClientRect() { return { top: 0, bottom: 0, left: 0, right: 0, width: 0, height: 0 }; },
    click() { fire(e, "click"); },
    play() { return Promise.resolve(); }, pause() {}, load() {},
    get innerHTML() { return ""; },
    set innerHTML(v) { e.children = []; },
  };
  return e;
}
function fire(el, kind) {
  const ev = { stopPropagation() {}, preventDefault() {}, target: el, key: "", clientX: 0, clientY: 0 };
  (el.handlers[kind] || []).forEach(fn => fn(ev));
}
function walk(el, out = []) { out.push(el); (el.children || []).forEach(c => walk(c, out)); return out; }
function findBtn(root, label) { return walk(root).find(e => e.tagName === "BUTTON" && e.textContent === label); }

const byId = {};
const document = {
  getElementById(id) { return byId[id] || (byId[id] = El("div")); },
  createElement: El, createTextNode: t => ({ textContent: t }),
  createDocumentFragment: () => El("div"),
  addEventListener() {}, removeEventListener() {},
  querySelector: () => null, querySelectorAll: () => [],
  body: El("body"), documentElement: El("html"),
  hidden: false, visibilityState: "visible",
};
const calls = [];
let ROUTES = {};
function fetchStub(url, opts) {
  opts = opts || {};
  const method = opts.method || "GET";
  calls.push({ method, url, body: opts.body });
  const key = method + " " + url.split("?")[0];
  const r = ROUTES[key];
  const payload = typeof r === "function" ? r(url, opts) : (r || {});
  return Promise.resolve({
    ok: true, status: 200,
    text: () => Promise.resolve(JSON.stringify(payload)),
    json: () => Promise.resolve(payload),
  });
}
const alerts = [];
const window = {
  alert: m => alerts.push(m), confirm: () => true, prompt: () => null,
  addEventListener() {}, removeEventListener() {},
  location: { hash: "", search: "", pathname: "/", href: "http://x/", origin: "http://x" },
  history: { pushState() {}, replaceState() {}, back() {} },
  matchMedia: () => ({ matches: false, addEventListener() {}, addListener() {} }),
  localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
  fetch: fetchStub, setTimeout, clearTimeout, setInterval: () => 0, clearInterval() {},
  requestAnimationFrame: fn => setTimeout(fn, 0),
  IntersectionObserver: class { observe() {} unobserve() {} disconnect() {} },
  navigator: { userAgent: "node", clipboard: { writeText: () => Promise.resolve() } },
  innerWidth: 1280, innerHeight: 800, scrollTo() {}, scrollY: 0,
  URL: global.URL, encodeURIComponent, decodeURIComponent,
};
const sandbox = {
  document, window, fetch: fetchStub, console,
  history: window.history, location: window.location,
  localStorage: window.localStorage, navigator: window.navigator,
  setTimeout, clearTimeout, setInterval: window.setInterval, clearInterval,
  requestAnimationFrame: window.requestAnimationFrame,
  IntersectionObserver: window.IntersectionObserver,
  alert: window.alert, confirm: window.confirm, prompt: window.prompt,
  matchMedia: window.matchMedia, innerWidth: 1280, innerHeight: 800,
  addEventListener() {}, URL: global.URL, Promise, JSON, Math, Date, Object, Array, String, Number,
};
const vm = require("vm");
vm.createContext(sandbox);
try { vm.runInContext(script, sandbox); } catch (e) { console.error("LOAD FAILED:", e.message); process.exit(1); }

const D = { byId, ROUTES: r => (ROUTES = r), calls, alerts, fire, walk, findBtn };

// ---- the drive ----

const groups = [
  { ref: "f1", faces: 1, items: 1, cover_url: "/api/face/1" },
  { ref: "f2", faces: 1, items: 1, cover_url: "/api/face/2" },
  { ref: "f3", faces: 1, items: 1, cover_url: "/api/face/3" },
];
let merged = false;
D.ROUTES({
  "GET /api/auth": { enabled: false, authed: true },
  "GET /api/stats": { faces: { faces: 3 }, items: 3 },
  "GET /api/items": { items: [] },
  "GET /api/people": () => ({
    people: [], clusters_total: merged ? 2 : 3, match: 0.8,
    clusters: merged
      ? [{ ref: "f1", faces: 2, items: 2, cover_url: "/api/face/1", merged: true }, groups[2]]
      : groups,
    progress: { visual: 3, done: 3, faces: 3, named: 0 },
  }),
  "POST /api/people/merge": () => { merged = true; return { ok: true, ref: "f1", faces: 2, items: 2 }; },
  "DELETE /api/people/merge": () => { merged = false; return { ok: true }; },
});

const people = D.byId["people"];
const head = D.byId["pagehead"];
const tick = () => new Promise(r => setTimeout(r, 0));
const labels = () => D.walk(people).filter(e => e.tagName === "BUTTON").map(e => e.textContent);
const cards = () => people.children.map(c => c.className);

(async () => {
  let fail = 0;
  const check = (ok, msg) => { console.log((ok ? "  ok   " : "  FAIL ") + msg); if (!ok) fail++; };

  D.fire(D.byId["peoplebtn"], "click");
  await tick(); await tick();
  console.log("1. People page, nothing merged");
  check(people.children.length === 3, "three cards rendered");
  check(labels().filter(l => l === "Merge…").length === 3, "every unnamed card offers Merge…  -> " + JSON.stringify(labels()));
  check(!labels().includes("Unmerge"), "no Unmerge offered yet");

  console.log("2. Pick the first group");
  D.fire(D.findBtn(people, "Merge…"), "click");
  await tick(); await tick();
  check(labels().join(",") === "Cancel,Merge here,Merge here",
    "picked card says Cancel, the others say Merge here -> " + JSON.stringify(labels()));
  check(cards()[0].includes("mergepick"), "picked card is highlighted -> " + JSON.stringify(cards()));
  const bar = D.walk(head).find(e => e.className === "mergebar");
  check(!!bar && /pick the group/.test(bar.textContent), "the page says what it is waiting for");
  check(!!D.findBtn(head, "Cancel"), "and offers a way out of it");

  console.log("3. Cancel puts it back");
  D.fire(D.findBtn(head, "Cancel"), "click");
  await tick(); await tick();
  check(labels().filter(l => l === "Merge…").length === 3, "back to three Merge… buttons");

  console.log("4. Pick, then merge into the second group");
  D.fire(D.findBtn(people, "Merge…"), "click");
  await tick(); await tick();
  D.fire(D.findBtn(people, "Merge here"), "click");
  await tick(); await tick(); await tick();
  const post = D.calls.filter(c => c.method === "POST" && c.url === "/api/people/merge").pop();
  check(!!post, "posted to /api/people/merge");
  check(post && post.body === JSON.stringify({ ref: "f1", into: "f2" }),
    "with the picked group and the target -> " + (post && post.body));
  check(/^Merged — 2 faces in 2 items/.test(D.alerts[D.alerts.length - 1]),
    "and said what came out -> " + JSON.stringify(D.alerts[D.alerts.length - 1]));

  console.log("5. The merged card");
  check(people.children.length === 2, "two cards now");
  const meta = D.walk(people.children[0]).find(e => e.className === "pmeta");
  check(meta && / · merged$/.test(meta.textContent), "says merged -> " + (meta && meta.textContent));
  check(labels().includes("Unmerge"), "offers Unmerge -> " + JSON.stringify(labels()));
  check(!labels().some(l => l === "Cancel" || l === "Merge here"), "and is out of merge mode");

  console.log("6. Unmerge");
  D.fire(D.findBtn(people, "Unmerge"), "click");
  await tick(); await tick(); await tick();
  const del = D.calls.filter(c => c.method === "DELETE").pop();
  check(del && del.url === "/api/people/merge?ref=f1", "deleted the right ref -> " + (del && del.url));
  check(people.children.length === 3, "back to three groups");

  console.log(fail ? "\nFAILED (" + fail + ")" : "\nALL OK");
  process.exit(fail ? 1 : 0);
})();
