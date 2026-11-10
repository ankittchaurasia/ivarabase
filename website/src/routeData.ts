import { defineRouteMiddleware } from '@astrojs/starlight/route-data';

// Hero buttons are set in frontmatter, where the base path isn't known. Prefix
// root-relative links ("/docs/docker/") with it, as sync-docs.mjs does for the docs.
export const onRequest = defineRouteMiddleware((context) => {
	const prefix = import.meta.env.BASE_URL.replace(/\/$/, '');
	const hero = context.locals.starlightRoute.entry.data.hero;
	if (!prefix || !hero?.actions) return;
	for (const action of hero.actions) {
		if (action.link.startsWith('/') && !action.link.startsWith('//') && !action.link.startsWith(prefix + '/')) {
			action.link = prefix + action.link;
		}
	}
});
