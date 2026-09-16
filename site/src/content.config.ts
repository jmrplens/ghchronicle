import { defineCollection } from "astro:content";
import { z } from "astro/zod";
import { docsLoader, i18nLoader } from "@astrojs/starlight/loaders";
import { docsSchema, i18nSchema } from "@astrojs/starlight/schema";

export const collections = {
	// `indexTables` names, by the heading of their first column, the tables on
	// a page that are indexes rather than references. src/lib/rehype-tables.mjs
	// reads it and styles/tables.css gives those a compact form on a phone
	// instead of the stacked one.
	docs: defineCollection({
		loader: docsLoader(),
		schema: docsSchema({
			extend: z.object({
				indexTables: z.array(z.string()).optional(),
			}),
		}),
	}),
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
