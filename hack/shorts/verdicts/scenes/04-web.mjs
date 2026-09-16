// Trigger the review job in the daemon build.sh started and screenshot the run page twice: top of the transcript, then the verdict and spend. Env: STEPS_URL, OUT (path prefix).
import { launch } from "cloakbrowser";

const { STEPS_URL, OUT } = process.env;
const browser = await launch({ headless: true });
// A phone's density: 540x960 CSS pixels at 2x is a 1080x1920 frame with text a viewer can read.
const page = await browser.newPage({ viewport: { width: 540, height: 960 }, deviceScaleFactor: 2 });
await page.goto(`${STEPS_URL}/p/review/jobs/review`);
await page.getByRole("button", { name: /trigger/i }).first().click();
await page.waitForURL(/\/runs\/[^/]+$/, { timeout: 60_000 });
// The title leads with the run's mark: ◐ while running, ✓ or ✗ once it is over. The live page streams steps in but keeps its title, so reload.
for (let i = 0; i < 120 && !/^[✓✗]/.test(await page.title()); i++) {
  await page.waitForTimeout(1000);
  await page.reload();
}
await page.waitForTimeout(500);
await page.screenshot({ path: `${OUT}-1.png` });
await page.evaluate(() => window.scrollBy(0, 700));
await page.waitForTimeout(300);
await page.screenshot({ path: `${OUT}-2.png` });
await browser.close();
