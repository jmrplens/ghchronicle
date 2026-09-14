import { defineCollection } from "astro:content";
import { z } from "astro/zod";
import { docsLoader, i18nLoader } from "@astrojs/starlight/loaders";
import { docsSchema, i18nSchema } from "@astrojs/starlight/schema";

export const collections = {
	docs: defineCollection({ loader: docsLoader(), schema: docsSchema() }),
	// The UI strings the overrides supply for themselves, on top of Starlight's
	// own: one file per locale in src/content/i18n, so a label is translated
	// where the other translations live instead of in a ternary on the locale.
	i18n: defineCollection({
		loader: i18nLoader(),
		schema: i18nSchema({
			extend: z.object({
				"ghc.breadcrumb.home": z.string().optional(),
			}),
		}),
	}),
};
