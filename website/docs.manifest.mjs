// The docs that make up the website, in sidebar order. The Markdown files in
// ../docs/ are the single source of truth: scripts/sync-docs.mjs copies them
// into src/content/docs/docs/ at build time, and astro.config.mjs builds the
// sidebar from this list. A file listed here that doesn't exist yet is skipped
// with a warning, so the site still builds.

export const REPO = 'https://github.com/ankittchaurasia/ivarabase';
export const BRANCH = 'main';

/** Where synced pages live inside src/content/docs/ (and so in the URL). */
export const DOCS_PREFIX = 'docs';

/** Where synced images are copied, inside public/ (and so in the URL). */
export const IMAGES_DIR = 'docs-images';

export const SECTIONS = [
	{
		label: 'Get started',
		items: [
			{ file: 'DOCKER.md', label: 'Install with Docker' },
			{ file: 'DEPLOY.md', label: 'Install without Docker' },
			{ file: 'PROXY.md', label: 'Behind a reverse proxy' },
			{ file: 'CONFIGURATION.md', label: 'Configuration' },
			{ file: 'UPGRADING.md', label: 'Upgrading' },
		],
	},
	{
		label: 'Guides',
		items: [
			{ file: 'AGENTS.md', label: 'AI agents & MCP' },
			{ file: 'DATABASE.md', label: 'Database' },
			{ file: 'AUTH.md', label: 'Auth' },
			{ file: 'APPLE.md', label: 'Sign in with Apple' },
			{ file: 'STORAGE.md', label: 'Storage' },
			{ file: 'REALTIME.md', label: 'Realtime' },
			{ file: 'FUNCTIONS.md', label: 'Functions' },
			{ file: 'WEBHOOKS.md', label: 'Database webhooks' },
			{ file: 'BACKUPS.md', label: 'Backups' },
			{ file: 'CONFIG-AS-CODE.md', label: 'Config as code' },
		],
	},
	{
		label: 'Compare & migrate',
		items: [
			{ file: 'COMPARISON.md', label: 'Comparison' },
			{ file: 'SUPABASE.md', label: 'Supabase compatibility' },
		],
	},
	{
		label: 'Project',
		items: [
			{ file: 'ARCHITECTURE.md', label: 'Architecture' },
			{ file: 'ROADMAP.md', label: 'Roadmap' },
		],
	},
];

/**
 * Old file names that links in the docs may still use while files are being
 * renamed. A link to the old name goes to the new page.
 */
export const ALIASES = {
	'CONFIG.md': 'CONFIG-AS-CODE.md',
	'PLAN.md': 'ARCHITECTURE.md',
};

/** Every listed file. */
export const ALL_DOCS = SECTIONS.flatMap((s) => s.items);

/** DOCKER.md -> "docker", CONFIG-AS-CODE.md -> "config-as-code". */
export function pageName(file) {
	return file.replace(/\.md$/i, '').toLowerCase();
}

/** The content-collection slug of a doc, e.g. "docs/docker". */
export function slugFor(file) {
	return `${DOCS_PREFIX}/${pageName(file)}`;
}

/** BASE env var normalised to "/" or "/something/". */
export function normalizeBase(base = '/') {
	let b = base.trim() || '/';
	if (!b.startsWith('/')) b = '/' + b;
	if (!b.endsWith('/')) b += '/';
	return b;
}
