import express from "express";
import fs from "fs";
import path from "path";
import { chromium } from "playwright";

const app = express();
app.use(express.json({ limit: "1mb" }));

const TEMPLATE = fs.readFileSync(path.join(process.cwd(), "template.html"), "utf-8");

app.get("/health", (req, res) => res.json({ ok: true }));

app.post("/render", async (req, res) => {
  const { width, height, state } = req.body || {};
  const W = Number(width) || 320;
  const H = Number(height) || 160;

  if (W > 1920 || H > 1080) return res.status(400).json({ error: "resolution too large" });

  const browser = await chromium.launch({ args: ["--no-sandbox", "--disable-dev-shm-usage"] });

  try {
    const page = await browser.newPage({ viewport: { width: W, height: H, deviceScaleFactor: 1 } });

    const html = TEMPLATE.replace(
      "<script>",
      `<script>window.__PAYLOAD__=${JSON.stringify({ width: W, height: H, state })};</script><script>`
    );

    await page.setContent(html, { waitUntil: "load" });

    await page.evaluate(async () => {
      if (document.fonts && document.fonts.ready) await document.fonts.ready;
    });

    const el = await page.$("#canvas");
    const png = await el.screenshot({ type: "png" });

    res.set("Content-Type", "image/png");
    res.send(png);
  } catch (e) {
    res.status(500).json({ error: String(e?.message || e) });
  } finally {
    await browser.close();
  }
});

app.listen(3000, "0.0.0.0", () => console.log("renderer-service :3000"));
