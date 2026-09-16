// Expressive Code options that cannot be written in astro.config.mjs.
//
// A plugin is a function, and the <Code> component (used by Card.astro and
// the home page) reads its configuration from this file, because what
// astro.config.mjs hands the integration has to be serialisable. The options
// that are plain values stay in astro.config.mjs; the integration merges the
// two.
import { defineEcConfig } from "@astrojs/starlight/expressive-code";

import { pluginContinuationLines } from "./src/lib/ec-continuation.mjs";

export default defineEcConfig({
	plugins: [pluginContinuationLines()],
});
