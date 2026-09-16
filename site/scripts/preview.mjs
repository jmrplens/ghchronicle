// Starting `astro preview` on a port nobody holds, for the gates that need a
// real server in front of a real browser.
//
// The port is never assumed. Astro's preview has no strict-port flag: when the
// port it is given is taken it prints "Port 4321 is in use, trying another one"
// and serves somewhere else. A gate that then measured port 4321 would be
// measuring another process. That happened on 2026-09-11 to the accessibility
// run, where nine pages passed against a stranger and the other thirteen
// failed with connection refused; a stale preview of an older build on that
// port would have passed all twenty-two. So the kernel is asked for a free
// port, and what the caller uses is the URL the preview announces on its
// "Local" line, not the number it was asked for.
//
// Used by scripts/run-pa11y.mjs and scripts/check-table-fit.mjs.
import { spawn } from "node:child_process";
import { createServer } from "node:net";
import process from "node:process";

const HOST = "127.0.0.1";
// Long enough for a cold start on a CI runner; the preview is ready in
// milliseconds once node is up.
const READY_TIMEOUT_MS = 60_000;
// Colour codes, which Astro writes even into a pipe.
const ANSI = /\u001b\[[0-9;]*m/g;
const LOCAL = /Local\s+(http:\/\/\S+)/;

/**
 * A port nobody holds. Another process can still take it before the preview
 * binds it, which is why the announced URL, not this number, is what a caller
 * points a browser at.
 *
 * @returns {Promise<number>}
 */
export function freePort() {
	return new Promise((resolvePort, reject) => {
		const server = createServer();
		server.once("error", reject);
		server.listen(0, HOST, () => {
			const { port } = server.address();
			server.close(() => resolvePort(port));
		});
	});
}

/**
 * Runs the preview in a process group of its own, so stopping it stops the
 * node process behind the pnpm wrapper too. Killing only the wrapper leaves
 * the server listening.
 *
 * @param {number} port the port to ask for
 * @returns {{ announced: Promise<URL>, stop: () => void }}
 */
export function startPreview(port) {
	const child = spawn(
		"pnpm",
		[
			"exec",
			"astro",
			"preview",
			"--host",
			HOST,
			"--port",
			String(port),
			"--ignore-lock",
		],
		{
			detached: true,
			// Astro 7.2 moves the preview to the background when it thinks an
			// agent started it, and a background preview outlives this script.
			env: { ...process.env, ASTRO_PREVIEW_BACKGROUND: "0" },
			stdio: ["ignore", "pipe", "pipe"],
		},
	);
	const stop = () => {
		try {
			process.kill(-child.pid, "SIGTERM");
		} catch {
			// Already gone, which is the state stop() is for.
		}
	};
	const announced = new Promise((resolveURL, reject) => {
		let output = "";
		const timer = setTimeout(
			() =>
				reject(
					new Error(
						`the preview announced no URL within ${READY_TIMEOUT_MS / 1000}s:\n${output}`,
					),
				),
			READY_TIMEOUT_MS,
		);
		const read = (chunk) => {
			output += chunk.toString().replace(ANSI, "");
			const match = LOCAL.exec(output);
			if (match) {
				clearTimeout(timer);
				resolveURL(new URL(match[1]));
			}
		};
		child.stdout.on("data", read);
		child.stderr.on("data", read);
		child.once("exit", (code) => {
			clearTimeout(timer);
			reject(
				new Error(
					`the preview exited with ${code} before announcing a URL:\n${output}`,
				),
			);
		});
	});
	return { announced, stop };
}
