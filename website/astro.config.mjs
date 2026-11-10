// @ts-check
import fs from 'node:fs';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import { REPO, SECTIONS, normalizeBase, slugFor } from './docs.manifest.mjs';

// Deployed at https://base.ivara.io; SITE and BASE can still be overridden:
//   SITE=https://docs.example.com BASE=/ npm run build
const SITE = process.env.SITE || 'https://base.ivara.io';
const BASE = normalizeBase(process.env.BASE);

const docsDir = fileURLToPath(new URL('../docs/', import.meta.url));

// The sidebar follows docs.manifest.mjs, leaving out files that don't exist yet.
const sidebar = SECTIONS.map((section) => ({
	label: section.label,
	items: section.items
		.filter((item) => fs.existsSync(docsDir + item.file))
		.map((item) => ({ label: item.label, slug: slugFor(item.file) })),
})).filter((section) => section.items.length > 0);

export default defineConfig({
	site: SITE,
	base: BASE,
	trailingSlash: 'ignore',
	integrations: [
		starlight({
			title: 'IvaraBase',
			description:
				'A self-hosted, Supabase-compatible backend that AI agents can run over MCP. Many projects on one server, one container.',
			logo: { src: './src/assets/logo.svg' },
			favicon: '/favicon.svg',
			social: [{ icon: 'github', label: 'GitHub', href: REPO }],
			editLink: { baseUrl: `${REPO}/edit/main/docs/` },
			customCss: ['@fontsource-variable/inter', './src/styles/theme.css'],
			sidebar,
			routeMiddleware: './src/routeData.ts',
			lastUpdated: false,
			credits: false,
			expressiveCode: {
				themes: ['github-dark-default', 'github-light-default'],
				styleOverrides: {
					borderRadius: '0.5rem',
					codeFontFamily:
						"ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, 'Liberation Mono', monospace",
				},
			},
		}),
	],
});
