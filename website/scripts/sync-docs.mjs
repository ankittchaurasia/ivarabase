#!/usr/bin/env node
// Copies ../docs/*.md and ../docs/images/* into the Starlight site.
//
//   ../docs/DOCKER.md     -> src/content/docs/docs/docker.md   (URL: <BASE>docs/docker/)
//   ../docs/images/x.png  -> public/docs-images/x.png          (URL: <BASE>docs-images/x.png)
//
// For each page it adds frontmatter (title from the first "# H1", which is
// removed from the body; description from the first paragraph; an editUrl on
// GitHub) and rewrites relative links so they work on the website:
//
//   FOO.md, FOO.md#x        -> <BASE>docs/foo/, <BASE>docs/foo/#x
//   ../README.md, ../x.yml  -> https://github.com/<repo>/blob/main/README.md ...
//   images/x.png            -> <BASE>docs-images/x.png
//
// <BASE> is the BASE environment variable (default "/"), the same one
// astro.config.mjs uses, so links work when the site is served from a sub-path.
//
// Both output folders are generated (and gitignored). Run by `npm run dev` and
// `npm run build`; run it alone with `npm run sync`.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import {
	ALIASES,
	ALL_DOCS,
	BRANCH,
	DOCS_PREFIX,
	IMAGES_DIR,
	REPO,
	normalizeBase,
	pageName,
} from '../docs.manifest.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const site = path.resolve(here, '..');
const repoRoot = path.resolve(site, '..');
const srcDocs = path.join(repoRoot, 'docs');
const srcImages = path.join(srcDocs, 'images');
const outDocs = path.join(site, 'src', 'content', 'docs', DOCS_PREFIX);
const outImages = path.join(site, 'public', IMAGES_DIR);

const BASE = normalizeBase(process.env.BASE);
const listed = new Set(ALL_DOCS.map((d) => d.file));

let warnings = 0;
function warn(msg) {
	warnings++;
	console.warn(`[sync-docs] warning: ${msg}`);
}

// ---------------------------------------------------------------- links

function rewriteTarget(target, fromFile) {
	if (!target || /^[a-z][a-z0-9+.-]*:/i.test(target) || target.startsWith('#') || target.startsWith('/')) {
		return target;
	}
	const hashAt = target.indexOf('#');
	const rawPath = hashAt === -1 ? target : target.slice(0, hashAt);
	const hash = hashAt === -1 ? '' : target.slice(hashAt);
	if (!rawPath) return target;

	// Resolve relative to the repo root, as GitHub would from docs/<file>.
	const resolved = path.posix.normalize(path.posix.join('docs', rawPath));
	if (resolved.startsWith('..')) {
		warn(`${fromFile}: link "${target}" points outside the repository; left as is`);
		return target;
	}

	// Another doc page.
	const m = /^docs\/([^/]+\.md)$/i.exec(resolved);
	if (m) {
		const file = ALIASES[m[1]] ?? m[1];
		if (listed.has(file)) {
			if (!fs.existsSync(path.join(srcDocs, file))) {
				warn(`${fromFile}: links to ${file}, which doesn't exist yet (the link will 404 until it does)`);
			}
			return `${BASE}${DOCS_PREFIX}/${pageName(file)}/${hash}`;
		}
		return `${REPO}/blob/${BRANCH}/${resolved}${hash}`;
	}

	// An image (or other file) under docs/images/.
	if (resolved.startsWith('docs/images/')) {
		const rel = resolved.slice('docs/images/'.length);
		if (!fs.existsSync(path.join(srcImages, rel))) warn(`${fromFile}: image ${resolved} not found`);
		return `${BASE}${IMAGES_DIR}/${rel}${hash}`;
	}

	// Anything else in the repo: show it on GitHub.
	const abs = path.join(repoRoot, resolved);
	const isDir = rawPath.endsWith('/') || (fs.existsSync(abs) && fs.statSync(abs).isDirectory());
	return `${REPO}/${isDir ? 'tree' : 'blob'}/${BRANCH}/${resolved.replace(/\/$/, '')}${hash}`;
}

const labels = Object.fromEntries(ALL_DOCS.map((d) => [d.file, d.label]));

function rewriteLine(line, file) {
	// A link whose text is just a file name, like [PROXY.md](PROXY.md), reads
	// better on the website as the page's name: [Behind a reverse proxy](...).
	line = line.replace(/\[`?(?:docs\/)?([A-Z][A-Z0-9-]*\.md)`?\]\((?=[^)\s]*\.md[#)])/g, (all, name) => {
		const label = labels[ALIASES[name] ?? name];
		return label ? `[${label}](` : all;
	});
	// Skip inline code spans while rewriting: split on backtick runs.
	return line
		.split(/(`+[^`]*`+)/)
		.map((part, i) => {
			if (i % 2 === 1) return part;
			return part
				// [text](target "title") and ![alt](target)
				.replace(/(\]\()(<[^>]+>|[^)\s]+)((?:\s+"[^"]*")?\))/g, (_, open, t, close) => {
					const bare = t.startsWith('<') ? t.slice(1, -1) : t;
					return open + rewriteTarget(bare, file) + close;
				})
				// <img src="..."> and <a href="..."> in inline HTML
				.replace(/(\b(?:src|href)=")([^"]+)(")/g, (_, a, t, b) => a + rewriteTarget(t, file) + b);
		})
		.join('')
		// [label]: target   (reference definitions)
		.replace(/^(\s{0,3}\[[^\]]+\]:\s*)(\S+)/, (_, a, t) => a + rewriteTarget(t, file));
}

// ------------------------------------------------------- page conversion

function plainText(md) {
	// Drop inline HTML tags, but not things like `<name>` inside code spans.
	md = md
		.split(/(`+[^`]*`+)/)
		.map((part, i) => (i % 2 ? part : part.replace(/<\/?[a-z][^>]*>/gi, '')))
		.join('');
	return md
		.replace(/!\[[^\]]*\]\([^)]*\)/g, '')
		.replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
		.replace(/\[([^\]]+)\]\[[^\]]*\]/g, '$1')
		.replace(/`([^`]+)`/g, '$1')
		.replace(/(\*\*|__)(.+?)\1/g, '$2')
		.replace(/(\*|_)(\S.*?)\1/g, '$2')
		.replace(/\s+/g, ' ')
		.trim();
}

function truncate(s, max = 200) {
	if (s.length <= max) return s;
	const cut = s.slice(0, max);
	return cut.slice(0, cut.lastIndexOf(' ')).replace(/[,;:.\s]+$/, '') + '…';
}

function convert(file, source) {
	const lines = source.replace(/\r\n/g, '\n').split('\n');
	let title = null;
	let description = '';
	const body = [];
	let fence = null; // the opening fence marker while inside a code block
	let para = null; // lines of the first paragraph, while collecting it

	for (const line of lines) {
		const fenceMatch = /^\s{0,3}(`{3,}|~{3,})/.exec(line);
		if (fence) {
			if (fenceMatch && fenceMatch[1][0] === fence[0] && fenceMatch[1].length >= fence.length) fence = null;
			body.push(line);
			continue;
		}
		if (fenceMatch) {
			fence = fenceMatch[1];
			if (para) {
				description ||= para.join(' ');
				para = null;
			}
			body.push(line);
			continue;
		}

		if (title === null) {
			const h1 = /^#\s+(.+?)\s*#*\s*$/.exec(line);
			if (h1) {
				title = plainText(h1[1]);
				continue; // drop the H1 from the body
			}
		}

		// The description is the first ordinary paragraph after the H1.
		if (title !== null && !description) {
			if (para) {
				if (line.trim() === '' || /^\s*(#|>|\||[-*+] |\d+\. |<)/.test(line)) {
					description = para.join(' ');
					para = null;
				} else {
					para.push(line);
				}
			} else if (line.trim() !== '' && !/^\s*(#|>|\||[-*+] |\d+\. |<|!\[|---)/.test(line)) {
				para = [line];
			}
		}

		body.push(rewriteLine(line, file));
	}
	if (para && !description) description = para.join(' ');

	if (title === null) {
		warn(`${file}: no "# H1" heading; using the file name as the title`);
		title = pageName(file);
	}
	description = truncate(plainText(description));

	// Trim blank lines left at the top after removing the H1.
	while (body.length && body[0].trim() === '') body.shift();

	const fm = [
		'---',
		'# Generated by scripts/sync-docs.mjs from docs/' + file + '. Edit that file, not this one.',
		`title: ${JSON.stringify(title)}`,
		...(description ? [`description: ${JSON.stringify(description)}`] : []),
		`editUrl: ${JSON.stringify(`${REPO}/edit/${BRANCH}/docs/${file}`)}`,
		'---',
		'',
	];
	return fm.join('\n') + body.join('\n').replace(/\s*$/, '\n');
}

// ------------------------------------------------------------------ main

function copyDir(from, to) {
	fs.mkdirSync(to, { recursive: true });
	let n = 0;
	for (const entry of fs.readdirSync(from, { withFileTypes: true })) {
		if (entry.name.startsWith('.')) continue;
		const a = path.join(from, entry.name);
		const b = path.join(to, entry.name);
		if (entry.isDirectory()) n += copyDir(a, b);
		else {
			fs.copyFileSync(a, b);
			n++;
		}
	}
	return n;
}

fs.rmSync(outDocs, { recursive: true, force: true });
fs.rmSync(outImages, { recursive: true, force: true });
fs.mkdirSync(outDocs, { recursive: true });

let pages = 0;
const missing = [];
for (const { file } of ALL_DOCS) {
	const src = path.join(srcDocs, file);
	if (!fs.existsSync(src)) {
		missing.push(file);
		warn(`docs/${file} doesn't exist yet; skipped (and left out of the sidebar)`);
		continue;
	}
	fs.writeFileSync(path.join(outDocs, `${pageName(file)}.md`), convert(file, fs.readFileSync(src, 'utf8')));
	pages++;
}

let images = 0;
if (fs.existsSync(srcImages)) images = copyDir(srcImages, outImages);
else fs.mkdirSync(outImages, { recursive: true });

const unlisted = fs.existsSync(srcDocs)
	? fs
			.readdirSync(srcDocs)
			.filter((f) => f.endsWith('.md') && !listed.has(f) && f !== 'README.md' && !(f in ALIASES))
	: [];
for (const f of unlisted) warn(`docs/${f} isn't in docs.manifest.mjs, so it isn't on the website`);

console.log(
	`[sync-docs] ${pages} pages, ${images} images synced (BASE=${BASE})` +
		(missing.length ? `; missing: ${missing.join(', ')}` : '') +
		(warnings ? `; ${warnings} warning(s)` : ''),
);
