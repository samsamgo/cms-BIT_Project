import express from "express";
import fs from "fs";
import path from "path";
import { chromium } from "playwright";

const app = express();
app.use(express.json({ limit: "1mb" }));

const TEMPLATE = fs.readFileSync(path.join(process.cwd(), "template.html"), "utf-8");

app.get("/health", (req, res) => res.json({ ok: true }));

let browserPromise = null;
async function getBrowser() {
  if (!browserPromise) {
    browserPromise = chromium.launch({
      args: ["--no-sandbox", "--disable-dev-shm-usage"]
    });
  }
  return browserPromise;
}

app.post("/render", async (req, res) => {
  try {
    const { width, height, state } = req.body || {};
    const W = Number(width) || 320;
    const H = Number(height) || 160;

    if (W > 1920 || H > 1080) {
      return res.status(400).json({ error: "resolution too large" });
    }
    if (!state || typeof state !== "object") {
      return res.status(400).json({ error: "state is required" });
    }

    const browser = await getBrowser();
    const context = await browser.newContext({
      viewport: { width: W, height: H },
      deviceScaleFactor: 1
    });

    const page = await context.newPage();

    const payloadScript = `<script>window.__PAYLOAD__=${JSON.stringify({ width: W, height: H, state })}</script>`;
    const html = TEMPLATE.includes("</head>")
      ? TEMPLATE.replace("</head>", `${payloadScript}</head>`)
      : `${payloadScript}${TEMPLATE}`;

    await page.setContent(html, { waitUntil: "domcontentloaded" });

    await page.waitForFunction(() => {
      const canvas = document.querySelector("#canvas");
      return canvas && canvas.offsetWidth > 0 && canvas.offsetHeight > 0;
    });

    await page.waitForFunction(() => window.__RENDER_READY__ === true);

    // 폰트 로딩 대기(픽셀 흔들림 방지)
    await page.evaluate(async () => {
      if (document.fonts && document.fonts.ready) {
        await document.fonts.ready;
      }
    });

    const el = await page.$("#canvas");
    if (!el) {
      await context.close();
      return res.status(500).json({ error: "template missing #canvas" });
    }

    const png = await el.screenshot({ type: "png" });

    await context.close();

    res.setHeader("Content-Type", "image/png");
    res.status(200).send(png);
  } catch (e) {
    res.status(500).json({ error: String(e?.message || e) });
  }
});

const PORT = Number(process.env.PORT) || 3000;
app.listen(PORT, () => console.log(`renderer listening on :${PORT}`));
