// The reader: one page of a PDF at a time, as wide as the screen, with the
// search terms marked in the PDF's text layer. ‹ › step from hit page to hit
// page, − + and the page field from page to page, the arrow keys too. The
// passage the page store has for a hit page is shown above the page; for
// scans without a text layer it is the only mark.
//
// The rendered page is shown as an image made from the canvas, so the
// browser's own "Save image" and "Copy image" take it, and the "Bild" link
// saves it under the name of the file and page.

const stage = document.querySelector(".stage");
const cfg = JSON.parse(stage.dataset.reader);
const pdfjsLib = window.pdfjsLib;
pdfjsLib.GlobalWorkerOptions.workerSrc = "/einreichen/static/pdfjs/pdf.worker.min.js";

const canvas = stage.querySelector("canvas");
const img = stage.querySelector(".page img");
const save = document.querySelector(".save");
const textLayer = stage.querySelector(".textLayer");
const loading = stage.querySelector(".loading");
const pageno = document.querySelector(".pageno");
const of = document.querySelector(".of");
const passage = document.querySelector(".passage");
const orig = document.querySelector(".orig");
const hitnav = document.querySelector(".hitnav");
const hitpos = document.querySelector(".hitpos");

const hits = cfg.hits || [];
const terms = (cfg.terms || []).map((t) => t.toLowerCase()).filter(Boolean);
let doc = null;
let current = 0;
let rendering = null;

function escapeRe(s) { return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"); }
// the last term also as the beginning of a word, as the search takes it
const termRe = terms.length ? new RegExp(terms.map((t, i) => {
	const p = t.split(/\s+/).map(escapeRe).join("\\s+");
	return i === terms.length - 1 && !t.includes(" ") ? p + "[\\p{L}\\p{N}]*" : p;
}).join("|"), "giu") : null;

function mark(div) {
	const text = div.textContent;
	if (!termRe || !text) return false;
	termRe.lastIndex = 0;
	if (!termRe.test(text)) return false;
	termRe.lastIndex = 0;
	const frag = document.createDocumentFragment();
	let last = 0;
	for (const m of text.matchAll(termRe)) {
		frag.append(text.slice(last, m.index));
		const mk = document.createElement("mark");
		mk.textContent = m[0];
		frag.append(mk);
		last = m.index + m[0].length;
	}
	frag.append(text.slice(last));
	div.textContent = "";
	div.append(frag);
	return true;
}

async function show(n) {
	n = Math.max(1, Math.min(doc.numPages, n));
	current = n;
	pageno.value = n;
	orig.href = cfg.file + "#page=" + n;
	const u = new URL(location.href);
	u.searchParams.set("seite", n);
	history.replaceState(null, "", u);

	const i = hits.indexOf(n);
	if (hits.length) {
		hitnav.hidden = false;
		hitpos.textContent = i >= 0 ? `Treffer ${i + 1} von ${hits.length}` : `${hits.length} Trefferseiten`;
	}
	const snip = cfg.snippets && cfg.snippets[n];
	passage.hidden = !snip;
	if (snip) passage.innerHTML = snip;

	if (rendering) rendering.cancel();
	img.hidden = true;
	canvas.hidden = false;
	save.hidden = true;
	const page = await doc.getPage(n);
	const width = Math.min(stage.clientWidth, 1400);
	const base = page.getViewport({ scale: 1 });
	const scale = width / base.width;
	const vp = page.getViewport({ scale });
	const dpr = window.devicePixelRatio || 1;
	canvas.width = Math.floor(vp.width * dpr);
	canvas.height = Math.floor(vp.height * dpr);
	canvas.style.width = Math.floor(vp.width) + "px";
	canvas.style.height = Math.floor(vp.height) + "px";
	const box = canvas.parentElement;
	box.style.width = canvas.style.width;
	box.style.height = canvas.style.height;
	rendering = page.render({
		canvasContext: canvas.getContext("2d"),
		viewport: vp,
		transform: dpr !== 1 ? [dpr, 0, 0, dpr, 0, 0] : null,
	});
	try {
		await rendering.promise;
	} catch (e) {
		if (e && e.name === "RenderingCancelledException") return;
		throw e;
	}
	rendering = null;
	loading.hidden = true;
	showImage(n);

	textLayer.textContent = "";
	textLayer.style.setProperty("--scale-factor", scale);
	const divs = [];
	await pdfjsLib.renderTextLayer({
		textContentSource: await page.getTextContent(),
		container: textLayer,
		viewport: vp,
		textDivs: divs,
	}).promise;
	let first = null;
	for (const d of divs) {
		if (mark(d) && !first) first = d;
	}
	if (first) first.scrollIntoView({ block: "center" });
	else window.scrollTo(0, 0);
}

// showImage puts the picture of page n in place of the canvas once it is
// encoded, unless another page came meanwhile.
function showImage(n) {
	canvas.toBlob((blob) => {
		if (!blob || n !== current || rendering) return;
		if (img.src) URL.revokeObjectURL(img.src);
		img.src = URL.createObjectURL(blob);
		img.style.width = canvas.style.width;
		img.style.height = canvas.style.height;
		img.decode().catch(() => {}).then(() => {
			if (n !== current || rendering) return;
			img.hidden = false;
			canvas.hidden = true;
			save.href = img.src;
			save.download = cfg.name.replace(/\.pdf$/i, "") + "-S" + n + ".png";
			save.hidden = false;
		});
	}, "image/png");
}

function step(what) {
	if (!doc) return;
	if (what === "prev") show(current - 1);
	else if (what === "next") show(current + 1);
	else if (what === "nexthit") {
		const h = hits.find((p) => p > current) ?? hits[0];
		if (h) show(h);
	} else if (what === "prevhit") {
		const h = [...hits].reverse().find((p) => p < current) ?? hits[hits.length - 1];
		if (h) show(h);
	}
}

document.addEventListener("click", (e) => {
	const b = e.target.closest("[data-go]");
	if (b) step(b.dataset.go);
});
pageno.addEventListener("change", () => show(parseInt(pageno.value, 10) || 1));
document.addEventListener("keydown", (e) => {
	if (e.target === pageno || e.metaKey || e.ctrlKey || e.altKey) return;
	if (e.key === "ArrowRight" || e.key === "PageDown") step(e.shiftKey ? "nexthit" : "next");
	else if (e.key === "ArrowLeft" || e.key === "PageUp") step(e.shiftKey ? "prevhit" : "prev");
	else if (e.key === "n") step("nexthit");
	else if (e.key === "p") step("prevhit");
	else return;
	e.preventDefault();
});
let resizeTimer;
window.addEventListener("resize", () => {
	clearTimeout(resizeTimer);
	resizeTimer = setTimeout(() => doc && show(current), 200);
});

try {
	doc = await pdfjsLib.getDocument({ url: cfg.file, disableAutoFetch: true }).promise;
	of.textContent = "von " + doc.numPages;
	pageno.max = doc.numPages;
	await show(cfg.page);
} catch (e) {
	loading.innerHTML = "";
	loading.append("Das PDF ließ sich hier nicht anzeigen. ");
	const a = document.createElement("a");
	a.href = cfg.file;
	a.textContent = "Das Original öffnen";
	loading.append(a);
}
