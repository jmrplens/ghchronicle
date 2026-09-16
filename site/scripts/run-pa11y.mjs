// pa11y against the preview this script started, and no other server.
//
// The old script started `astro preview` and pointed pa11y-ci at the URLs in
// .pa11yci.json, all of them on port 4321. Astro's preview has no strict-port
// flag: when 4321 is taken it prints "Port 4321 is in use, trying another one"
// and serves somewhere else, and pa11y then audited whatever happened to be
// answering on 4321. That happened on 2026-09-11, when nine pages passed
// against another process and the other thirteen failed with connection
// refused; a stale preview of an older build on that port would have passed
// all twenty-two.
//
// So the port is never assumed; scripts/preview.mjs owns that, and the URL the
// preview announces on its "Local" line is the one pa11y is given: the list in
// .pa11yci.json keeps its paths and takes that origin.
//
// Usage: node scripts/run-pa11y.mjs
import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import process from "node:process";

import { freePort, startPreview } from "./preview.mjs";

// retarget keeps each configured page and moves it to the origin the preview
// actually serves.
function retarget(config, origin) {
	return {
		...config,
		urls: config.urls.map((raw) => {
			const url = new URL(raw);
			return new URL(url.pathname + url.search, origin).href;
		}),
	};
}

function runPa11y(configPath, env) {
	return new Promise((resolveCode) => {
		const child = spawn("pnpm", ["exec", "pa11y-ci", "--config", configPath], {
			env,
			stdio: "inherit",
		});
		child.once("exit", (code, signal) => resolveCode(code ?? (signal ? 1 : 0)));
	});
}

const { chromium } = await import("playwright");
const env = {
	...process.env,
	PUPPETEER_EXECUTABLE_PATH: chromium.executablePath(),
};

const config = JSON.parse(readFileSync(".pa11yci.json", "utf8"));
const preview = startPreview(await freePort());
for (const signal of ["SIGINT", "SIGTERM"]) {
	process.once(signal, () => {
		preview.stop();
		process.exit(1);
	});
}

const scratch = mkdtempSync(join(tmpdir(), "pa11y-"));
let exitCode = 1;
try {
	const base = await preview.announced;
	// The announced server is the one this script started. It must answer
	// the first page before a browser is pointed at the rest.
	const probe = await fetch(
		new URL(new URL(config.urls[0]).pathname, base.origin),
		{
			signal: AbortSignal.timeout(10_000),
		},
	);
	if (!probe.ok) {
		throw new Error(
			`the preview at ${base.origin} answered ${probe.status} for the first page`,
		);
	}
	console.log(`[pa11y] auditing the preview at ${base.origin}`);
	const configPath = join(scratch, "pa11yci.json");
	writeFileSync(
		configPath,
		JSON.stringify(retarget(config, base.origin), null, "\t"),
	);
	exitCode = await runPa11y(configPath, env);
} catch (error) {
	console.error(`[pa11y] ${error.message}`);
} finally {
	preview.stop();
	rmSync(scratch, { recursive: true, force: true });
}
process.exit(exitCode);
