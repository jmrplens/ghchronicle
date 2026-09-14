// The types Astro generates for this project: the typed content collections
// behind `astro:content` and the environment of `astro/client`.
//
// tsconfig.json lists .astro/types.d.ts in `include` and .astro in `exclude`,
// and the exclude wins, so without this reference getCollection() is typed
// from astro/client's untyped fallback and every entry is `any`. A reference
// path is not subject to `exclude`, which is why it is here and not a second
// pattern in the tsconfig.
/// <reference path="../.astro/types.d.ts" />
