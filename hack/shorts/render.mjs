// Screenshot an HTML slide at short-video size: node render.mjs <file.html> <out.png>
import { launch } from "cloakbrowser";
import { resolve } from "node:path";

const [html, out] = process.argv.slice(2);
const browser = await launch({ headless: true });
const page = await browser.newPage();
await page.setViewportSize({ width: 1080, height: 1920 });
await page.goto("file://" + resolve(html));
// A screenshot before the fonts resolve comes out in the fallback serif.
await page.evaluate(() => document.fonts.ready);
await page.waitForTimeout(300);
await page.screenshot({ path: out });
await browser.close();
