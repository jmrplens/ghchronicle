import { defineCollection } from "astro:content";
import { z } from "astro/zod";
import { docsLoader, i18nLoader } from "@astrojs/starlight/loaders";
import { docsSchema, i18nSchema } from "@astrojs/starlight/schema";

export const collections = {
	// `compactTables` names, by the heading of their first column, the tables on
	// a page that are not references: an index, a closed set, a glossary, any
	// table whose row is not a name you look up carrying more than one fact
	// about it. src/lib/rehype-tables.mjs reads it and styles/tables.css gives
	// those a compact line on a phone instead of the stacked card. It was
	// called `indexTables` while an index was the only case.
	//
	// `defaultColumns` names, for a table that stays a reference, which of its
	// other columns holds the row's default: `table` is the first column's
	// heading (which table), `default` is the heading of the column to hoist
	// beside its label (which column). A list of pairs rather than a map keyed
	// by the heading text itself, because a map key is compared for i18n parity
	// by NAME (scripts/check-i18n-parity.mjs walks a mapping's keys, not just
	// its values), and a heading is translated: "Key" in English is "Clave" in
	// Spanish, so a `defaultColumns: {Key: Default}` twin would need
	// `{Clave: "Por omisión"}` and the gate would read that as two different
	// frontmatter shapes. Both `table` and `default` are fixed field names
	// here, identical in every locale; only their values, which nobody
	// compares, are the translation.
	//
	// `plainTables` names, again by the heading of their first column, the
	// tables whose first cell is not the row's name: a sentence that happens to
	// hold a code span, as configuration/index.mdx's `Remembers` and `Message`
	// tables do. src/lib/rehype-tables.mjs leaves those cells unmarked, so
	// styles/tables.css sizes them as the body text they are instead of as a
	// card title. A list of scalars like `compactTables`, and translated the same
	// way: only the values differ between the twins.
	docs: defineCollection({
		loader: docsLoader(),
		schema: docsSchema({
			extend: z.object({
				compactTables: z.array(z.string()).optional(),
				defaultColumns: z
					.array(z.object({ table: z.string(), default: z.string() }))
					.optional(),
				plainTables: z.array(z.string()).optional(),
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
