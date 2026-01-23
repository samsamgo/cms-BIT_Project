import express from "express";
import bodyParser from "body-parser";
import puppeteer from "puppeteer";
import fs from "fs/promises";
import path from "path";

const app = express();
app.use(bodyParser.json({ limit: "2mb" }));

const PORT = process.env.PORT || 3000;
const TEMPLATE_PATH =
  process.env.TEMPLATE_PATH || path.join(process.cwd(), "template.html");

let browser = null;

async function ensureBrowser() {
  if (browser) return browser;
  browser = await puppeteer.launch({
    headless: "new",
    args: ["--no-sandbox", "--disable-setuid-sandbox", "--disable-dev-shm-usage"],
  });
  return browser;
}

app.get("/health", (req, res) => {
  res.json({ ok: true });
});

app.post("/render", async (req, res) => {
  let page = null;
  try {
    const { width, height, state } = req.body || {};

    const w = Number(width);
    const h = Number(height);

    if (!Number.isFinite(w) || !Number.isFinite(h) || w <= 0 || h <= 0) {
      return res.status(400).json({ error: "width/height must be positive numbers" });
    }
    if (typeof state !== "object" || state === null) {
      return res.status(400).json({ error: "state must be an object" });
    }

    const html = await fs.readFile(TEMPLATE_PATH, "utf-8");
    const b = await ensureBrowser();
    page = await b.newPage();

    await page.setViewport({
      width: Math.round(w),
      height: Math.round(h),
      deviceScaleFactor: 1,
    });

    // payload 안전 주입 (script 문자열 치환 금지)
    const payload = { width: Math.round(w), height: Math.round(h), state };
    await page.evaluateOnNewDocument((p) => {
      window.__PAYLOAD__ = p;
    }, payload);

    await page.setContent(html, { waitUntil: "domcontentloaded" });

    // 흔들림 방지 강제
    await page.addStyleTag({
      content: `
        * { animation: none !important; transition: none !important; }
        html, body { margin:0 !important; padding:0 !important; overflow:hidden !important; }
      `,
    });

    // 폰트 로드 완료 대기
    await page.evaluate(async () => {
      if (document.fonts && document.fonts.ready) {
        await document.fonts.ready;
      }
    });

    // 레이아웃 안정화: 1 frame 대기
    await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => r())));

    const canvas = await page.$("#canvas");
    if (!canvas) {
      return res.status(500).json({ error: "template missing #canvas" });
    }

    const png = await canvas.screenshot({ type: "png" });

    res.setHeader("Content-Type", "image/png");
    res.setHeader("Cache-Control", "no-store");
    res.send(png);
  } catch (err) {
    console.error(err);
    res.status(500).json({ error: String(err?.message || err) });
  } finally {
    if (page) {
      try { await page.close(); } catch {}
    }
  }
});

process.on("SIGINT", async () => {
  try { if (browser) await browser.close(); } catch {}
  process.exit(0);
});
process.on("SIGTERM", async () => {
  try { if (browser) await browser.close(); } catch {}
  process.exit(0);
});

app.listen(PORT, () => {
  console.log(`renderer-service listening on ${PORT}`);
  console.log(`template: ${TEMPLATE_PATH}`);
});
