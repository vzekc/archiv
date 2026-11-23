// The views: pictures open in a lightbox, other items in a panel beside
// the view on wide screens (on phones the item page opens), "Mehr laden"
// appends the next page of a grid, and "Link kopieren" copies the
// citable link. Without JavaScript every card is a plain link.

const wide = () => window.matchMedia("(min-width: 60em)").matches;
const plainClick = (e) => !(e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey);

// ------------------------------------------------------------ lightbox

let lbx = null;

function openLightbox(start) {
	const cards = [...document.querySelectorAll("[data-img]")];
	let i = cards.indexOf(start);
	if (i < 0) return;
	lbx = document.createElement("div");
	lbx.className = "lbx";
	lbx.innerHTML = `<div class="stage"><img alt=""></div>
		<button class="nav prev" aria-label="voriges Bild">‹</button><button class="nav next" aria-label="nächstes Bild">›</button>
		<div class="bar"><span class="cap"></span><span class="pos"></span><a class="goto" href="#">Zum Objekt</a><a class="orig" href="#">Original</a><button class="close">✕</button></div>`;
	document.body.append(lbx);
	document.body.style.overflow = "hidden";
	const img = lbx.querySelector("img");
	const show = () => {
		const c = cards[i];
		img.src = c.dataset.img;
		lbx.querySelector(".cap").textContent = c.getAttribute("title") || "";
		lbx.querySelector(".pos").textContent = `${i + 1} / ${cards.length}`;
		lbx.querySelector(".orig").href = c.dataset.img;
		// on the item page the picture's own link is the file: no way further
		const goto = lbx.querySelector(".goto");
		goto.href = c.getAttribute("href");
		goto.hidden = c.getAttribute("href") === c.dataset.img;
		// the next one ahead of time
		if (cards[i + 1]) new Image().src = cards[i + 1].dataset.img;
	};
	const step = (d) => { i = (i + d + cards.length) % cards.length; show(); };
	lbx.querySelector(".prev").onclick = () => step(-1);
	lbx.querySelector(".next").onclick = () => step(1);
	lbx.querySelector(".close").onclick = closeLightbox;
	lbx.querySelector(".stage").onclick = (e) => { if (e.target !== img) closeLightbox(); };
	let x0 = null;
	lbx.addEventListener("touchstart", (e) => { x0 = e.touches[0].clientX; }, { passive: true });
	lbx.addEventListener("touchend", (e) => {
		if (x0 === null) return;
		const dx = e.changedTouches[0].clientX - x0;
		if (Math.abs(dx) > 50) step(dx < 0 ? 1 : -1);
		x0 = null;
	});
	lbx.keys = (e) => {
		if (e.key === "Escape") closeLightbox();
		else if (e.key === "ArrowLeft") step(-1);
		else if (e.key === "ArrowRight") step(1);
	};
	document.addEventListener("keydown", lbx.keys);
	show();
}

function closeLightbox() {
	if (!lbx) return;
	document.removeEventListener("keydown", lbx.keys);
	lbx.remove();
	lbx = null;
	document.body.style.overflow = "";
}

// --------------------------------------------------------------- panel

const panel = document.getElementById("panel");

// on the search page the term goes along, for the finds in the item's PDFs
const searchTerm = location.pathname === "/suche" ? new URLSearchParams(location.search).get("q") : null;

async function openPanel(href) {
	if (searchTerm) href += (href.includes("?") ? "&" : "?") + "q=" + encodeURIComponent(searchTerm);
	const res = await fetch(href + (href.includes("?") ? "&" : "?") + "teil=panel");
	if (!res.ok) { location.href = href; return; }
	panel.querySelector(".pbody").innerHTML = await res.text();
	panel.hidden = false;
	panel.scrollTop = 0;
}

if (panel) {
	panel.querySelector(".close").onclick = () => { panel.hidden = true; };
	document.addEventListener("keydown", (e) => { if (e.key === "Escape" && !lbx) panel.hidden = true; });
}

// ---------------------------------------------------------- the grid

// The next page of a grid loads by itself when its end comes near; the
// link stays for loading by hand.
let loadingMore = false;
async function loadMore(more) {
	if (loadingMore) return;
	loadingMore = true;
	more.textContent = "…";
	try {
		const res = await fetch(more.getAttribute("href") + "&teil=raster");
		const doc = new DOMParser().parseFromString(await res.text(), "text/html");
		document.getElementById("grid").append(...doc.querySelectorAll("#grid > .card"));
		const next = doc.querySelector("[data-more]");
		if (next) more.setAttribute("href", next.getAttribute("href")), more.textContent = "Mehr laden";
		else more.parentElement.remove();
	} catch {
		more.textContent = "Mehr laden";
	}
	loadingMore = false;
	// still at the end (a tall screen): the next page right away
	const r = more.isConnected && more.getBoundingClientRect();
	if (r && r.top < window.innerHeight + 800) loadMore(more);
}

const moreLink = document.querySelector("[data-more]");
if (moreLink && "IntersectionObserver" in window) {
	new IntersectionObserver((entries) => {
		if (entries.some((x) => x.isIntersecting)) loadMore(moreLink);
	}, { rootMargin: "800px 0px" }).observe(moreLink);
}

// ------------------------------------------------------------- clicks

document.addEventListener("click", async (e) => {
	const copy = e.target.closest("[data-copy]");
	if (copy) {
		e.preventDefault();
		try {
			await navigator.clipboard.writeText(copy.dataset.copy);
			copy.textContent = "Link kopiert";
		} catch { location.href = copy.getAttribute("href"); }
		return;
	}
	const unfold = e.target.closest("[data-unfold]");
	if (unfold) {
		unfold.closest(".refine").classList.add("all");
		unfold.remove();
		return;
	}
	const more = e.target.closest("[data-more]");
	if (more) {
		e.preventDefault();
		loadMore(more);
		return;
	}
	// a click beside the open panel closes it, unless it opens another item
	if (panel && !panel.hidden && !e.target.closest("#panel") && !lbx &&
		!e.target.closest(".card, [data-panel], [data-item]")) {
		panel.hidden = true;
		if (!e.target.closest("a, button, summary, input, select")) return;
	}
	// a search hit: its links lead to the file, the rest of the row to the item
	const row = e.target.closest("[data-item]");
	if (row && !e.target.closest("a") && plainClick(e) && !getSelection().toString()) {
		const href = row.dataset.item;
		if (panel && wide()) openPanel(href);
		else location.href = href + (searchTerm ? "?q=" + encodeURIComponent(searchTerm) : "");
		return;
	}
	const card = e.target.closest(".card, [data-panel]");
	if (!card || !plainClick(e)) return;
	if (card.dataset.img !== undefined && card.dataset.img !== "") {
		e.preventDefault();
		openLightbox(card);
		return;
	}
	if (panel && wide() && card.getAttribute("href").startsWith("/a/")) {
		e.preventDefault();
		openPanel(card.getAttribute("href"));
	}
});

// ------------------------------------------------------- search results

// The results belong to the term they were found for: once the term in the
// search box changes, they go until the new search is sent.
const results = document.getElementById("results");
const qbox = document.querySelector("form.q input[name=q]");
if (results && qbox) {
	const shown = qbox.value.trim();
	const stale = document.getElementById("stale");
	const controls = document.querySelector(".sq");
	qbox.addEventListener("input", () => {
		const changed = qbox.value.trim() !== shown;
		results.hidden = changed;
		if (controls) controls.hidden = changed;
		if (stale) stale.hidden = !changed;
	});
}

// #bearbeiten opens the edit form of a tag, on arrival and when the pencil
// of a filter leads to the page already open
function openEdit() {
	const ed = document.getElementById("bearbeiten");
	if (!ed) return false;
	ed.open = true;
	ed.scrollIntoView({ block: "start" });
	ed.querySelector("input[name=name]")?.focus({ preventScroll: true });
	return true;
}
if (location.hash === "#bearbeiten") openEdit();
document.addEventListener("click", (e) => {
	const pen = e.target.closest("a.ed");
	if (!pen || !plainClick(e)) return;
	const to = new URL(pen.href);
	const here = new URL(location.href);
	here.searchParams.delete("ok");
	if (to.hash === "#bearbeiten" && to.pathname === here.pathname && to.search === here.search && openEdit()) {
		e.preventDefault();
		history.replaceState(null, "", to.pathname + to.search + to.hash);
	}
});
