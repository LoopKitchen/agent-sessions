// Browser regressions for the continuous reader's loader. The server renders
// whole turns; the script only appends pages, retries a failed fetch and
// reveals the element a fragment names. Run with:
//
//   NODE_PATH=/opt/homebrew/lib/node_modules node --test server/web/conversation.browser.cjs
//
// Playwright's own cached Chromium is used unless CHROMIUM_PATH names an
// executable. On a Mac with the cache under ~/Library/Caches/ms-playwright
// the executable is the browser inside the app bundle, for example
// ~/Library/Caches/ms-playwright/chromium-1234/chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing
const { test } = require("node:test");
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { chromium } = require("playwright");

const script = readFileSync(`${__dirname}/static/conversation.js`, "utf8");

// turn renders one section the way partials.html's "turn" template does:
// a prompt bubble, collapsed work with a tool row, and an answer slot.
const turn = (index, prompt, extra = "") => `<section class="turn turn-answered" id="t${index}" data-turn="${index}" data-thread="" data-outcome="answered">
<article class="blk blk-prompt" id="ev-p${index}" data-event-id="p${index}"><div class="gutter"><a class="el" href="#ev-p${index}" title="timestamp">+1m</a></div><div class="body"><div class="who">User</div><div class="markdown"><p>${prompt}</p></div></div></article>
<details class="work-summary"><summary>Worked 10s</summary><article class="blk blk-tool" id="ev-c${index}" data-event-id="c${index}"><div class="body"><details class="tool"><summary>Bash</summary><pre class="txt">tool output ${index}</pre></details></div></article>${extra}</details>
<article class="blk blk-assistant" id="ev-f${index}" data-event-id="f${index}"><div class="body"><div class="markdown"><p>answer ${index}</p></div></div></article>
</section>`;
const agent = (index, content) => `<details class="grp grp-agent" id="a-agent-${index}"><summary class="grp-head">subagent</summary><div class="grp-body">${content}</div></details>`;
const pageHTML = (content, next = "") => `<div class="session-layout"><div class="transcript">${content}</div><div class="transcript-next" data-next="${next}"><span role="status"></span><button class="transcript-retry" hidden>Retry</button></div><aside class="session-inspector"><div class="artifacts"><a href="/sessions/s/conversation?file=1#inspector-h">file</a></div></aside></div>`;

async function browserTest(run) {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ["--no-sandbox"] });
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on("pageerror", error => errors.push(error.message));
    await run(page);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
}

async function mount(page, initial, pages = {}, hash = "") {
  await page.route("http://reader.test/**", route => {
    const url = new URL(route.request().url());
    const content = url.pathname.endsWith("/conversation") ? initial : pages[url.searchParams.get("after")];
    if (!url.pathname.endsWith("/conversation")) assert.equal(url.searchParams.get("reader"), "1");
    return route.fulfill({ status: content ? 200 : 500, contentType: "text/html", body: content || "Failed" });
  });
  await page.goto(`http://reader.test/sessions/s/conversation${hash}`);
  await page.evaluate(() => {
    window.IntersectionObserver = class {
      constructor(callback) { window.loadNext = () => callback([{ isIntersecting: true }]); }
      observe() {}
    };
    document.querySelector(".transcript-next").getBoundingClientRect = () => ({ top: 999999 });
  });
  await page.addScriptTag({ content: script });
}

test("pages of whole turns append in order and a repeated turn renders once", () => browserTest(async page => {
  const initial = pageHTML(turn(0, "first") + turn(1, "second"), "/sessions/s?after=1");
  const a = pageHTML(turn(1, "second again") + turn(2, "third"), "/sessions/s?after=2");
  const b = pageHTML(turn(3, "fourth"));
  await mount(page, initial, { 1: a, 2: b });
  assert.deepEqual(await page.locator(".transcript > .turn").evaluateAll(nodes => nodes.map(n => n.id)), ["t0", "t1"]);
  await page.evaluate(() => window.loadNext());
  await page.waitForSelector("#t2", { state: "attached" });
  assert.deepEqual(await page.locator(".transcript > .turn").evaluateAll(nodes => nodes.map(n => n.id)), ["t0", "t1", "t2"]);
  assert.equal(await page.locator("#t1 .markdown").first().textContent(), "second");
  await page.evaluate(() => window.loadNext());
  await page.waitForSelector("#t3", { state: "attached" });
  assert.deepEqual(await page.locator(".transcript > .turn").evaluateAll(nodes => nodes.map(n => n.id)), ["t0", "t1", "t2", "t3"]);
  // The script assembled nothing: every turn's work is still inside its own
  // section, collapsed as the server rendered it.
  assert.equal(await page.locator(".turn > .work-summary").count(), 4);
  assert.equal(await page.locator(".turn > .work-summary[open]").count(), 0);
  assert.equal(await page.locator(".transcript-next").getAttribute("data-next"), "");
  assert.match(await page.locator(".transcript-next [role=status]").textContent(), /End of captured conversation/);
}));

test("a fragment naming a turn on a later page keeps loading until it arrives, then opens its details", () => browserTest(async page => {
  const initial = pageHTML(turn(0, "first"), "/sessions/s?after=0");
  const a = pageHTML(turn(1, "second"), "/sessions/s?after=1");
  const b = pageHTML(turn(2, "third", agent(2, `<article class="blk blk-assistant" id="ev-ag2" data-event-id="ag2"><div class="body">report</div></article>`)));
  await mount(page, initial, { 0: a, 1: b }, "#ev-ag2");
  await page.waitForSelector("#ev-ag2", { state: "attached" });
  await page.waitForFunction(() => document.querySelector("#t2 > .work-summary").open && document.querySelector("#a-agent-2").open);
  assert.equal(await page.locator("#ev-ag2").isVisible(), true);
  assert.equal(await page.locator(".turn").count(), 3);
  await page.evaluate(() => { location.hash = "ev-c0"; });
  await page.waitForFunction(() => document.querySelector("#t0 > .work-summary").open);
}));

test("a failed page fetch preserves the loaded turns and a retry continues without duplication", () => browserTest(async page => {
  const pages = {};
  await mount(page, pageHTML(turn(0, "first"), "/sessions/s?after=0"), pages);
  await page.evaluate(() => window.loadNext());
  await page.waitForSelector(".transcript-retry:not([hidden])");
  assert.equal(await page.locator(".transcript > .turn").count(), 1);
  assert.match(await page.locator(".transcript-next [role=status]").textContent(), /Could not load/);
  pages[0] = pageHTML(turn(1, "second"));
  await page.locator(".transcript-retry").click();
  await page.waitForSelector("#t1", { state: "attached" });
  assert.deepEqual(await page.locator(".transcript > .turn").evaluateAll(nodes => nodes.map(n => n.id)), ["t0", "t1"]);
}));

test("scrolling near an already-observed continuation resumes loading", () => browserTest(async page => {
  const initial = pageHTML(turn(0, "first"), "/sessions/s?after=0");
  const a = pageHTML(turn(1, "second"), "/sessions/s?after=1");
  const b = pageHTML(turn(2, "third"));
  await mount(page, initial, { 0: a, 1: b });
  await page.evaluate(() => window.loadNext());
  await page.waitForSelector("#t1", { state: "attached" });
  await page.evaluate(() => {
    document.querySelector(".transcript-next").getBoundingClientRect = () => ({ top: 100 });
    window.dispatchEvent(new Event("scroll"));
  });
  await page.waitForSelector("#t2", { timeout: 1500, state: "attached" });
  assert.equal(await page.locator(".transcript-next").getAttribute("data-next"), "");
}));

test("the inspector swaps in place and keeps the reader's scroll position", () => browserTest(async page => {
  const long = "captured answer ".repeat(3000) + "END OF ANSWER";
  const initial = pageHTML(turn(0, "request") + turn(1, long));
  await mount(page, initial);
  assert.equal(await page.locator("#t1 .blk-prompt .markdown").textContent(), long);
  await page.setViewportSize({ width: 1280, height: 600 });
  await page.evaluate(() => window.scrollTo(0, 300));
  const before = await page.evaluate(() => window.scrollY);
  await page.locator(".artifacts a").evaluate(node => node.click());
  await page.waitForFunction(() => location.search === "?file=1");
  assert.equal(await page.evaluate(() => window.scrollY), before);
  assert.equal(await page.locator(".transcript > .turn").count(), 2);
}));

test("an empty first page gives way to the turns a later page holds", () => browserTest(async page => {
  const initial = pageHTML(`<p class="empty">Nothing to show in this window.</p>`, "/sessions/s?after=0");
  const a = pageHTML(turn(1, "second"));
  await mount(page, initial, { 0: a });
  await page.evaluate(() => window.loadNext());
  await page.waitForSelector("#t1", { state: "attached" });
  assert.equal(await page.locator(".transcript > .empty").count(), 0);
}));
