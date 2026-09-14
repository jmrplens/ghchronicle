// The Spanish index for language models. See src/lib/llms.mjs for what it
// lists and why it is generated rather than written.
import type { APIRoute } from "astro";

import { renderIndex } from "../../lib/llms.mjs";

export const GET: APIRoute = async () =>
	new Response(await renderIndex("es"), {
		headers: { "content-type": "text/plain; charset=utf-8" },
	});
