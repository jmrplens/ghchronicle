// Every Spanish page concatenated, as the same markdown the twins serve, in
// sidebar order. The one fetch that gets a model the whole site.
import type { APIRoute } from "astro";

import { renderFull } from "../../lib/llms.mjs";

export const GET: APIRoute = async () =>
	new Response(await renderFull("es"), {
		headers: { "content-type": "text/plain; charset=utf-8" },
	});
