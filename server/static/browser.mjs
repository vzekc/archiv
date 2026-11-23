// The moderators' archive browser: a file manager over the catalog. The
// tree on the left holds the facets, the old folders, "Unsortiert" and the
// storage; the right pane shows a node's items as a list, a table or tiles.
// Every operation is one batch on the server (POST /api/batch) and can be
// undone from the session panel; other browsers learn about it through
// /api/events.

const $ = (sel, el = document) => el.querySelector(sel);
const csrf = $("meta[name=csrf]").content;
const editing = $("meta[name=editing]").content === "1";

const state = {
	node: null,        // NodeContent.node of the open node
	items: [],
	total: 0,
	sel: new Set(),    // selected item ids
	anchor: null,      // last clicked item, for shift ranges
	view: localGet("view") || "list",
	clip: null,        // {mode: "cut"|"copy", items: [], revs: [], from: tagId}
	session: [],       // [{batch, label, at}]
	loading: false,
};

function localGet(k) {
	try { return localStorage.getItem("browser." + k); } catch { return null; }
}
function localSet(k, v) {
	try { localStorage.setItem("browser." + k, v); } catch { /* private window */ }
}

async function api(path, body) {
	const opt = body === undefined ? {} : {
		method: "POST",
		headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
		body: JSON.stringify(body),
	};
	const res = await fetch(path, opt);
	const data = await res.json().catch(() => ({ error: "Antwort unlesbar (" + res.status + ")" }));
	if (!res.ok) throw new Error(data.error || res.statusText);
	return data;
}

// ------------------------------------------------------------- dialogs

const dlg = $("#dlg");

// ask shows a dialog with optional text fields and buttons; resolves with
// {button, values} or null when cancelled.
function ask({ title, text = "", fields = [], buttons = [["ok", "OK"]], danger = false }) {
	return new Promise((resolve) => {
		dlg.replaceChildren();
		const form = document.createElement("form");
		form.method = "dialog";
		const h = document.createElement("h3");
		h.textContent = title;
		form.append(h);
		if (text) {
			const p = document.createElement("p");
			p.textContent = text;
			form.append(p);
		}
		const inputs = fields.map((f) => {
			const label = document.createElement("label");
			label.textContent = f.label;
			const input = document.createElement("input");
			input.value = f.value || "";
			input.placeholder = f.placeholder || "";
			label.append(input);
			form.append(label);
			return input;
		});
		const bar = document.createElement("div");
		bar.className = "buttons";
		let chosen = null;
		for (const [value, label] of buttons) {
			const b = document.createElement("button");
			b.value = value;
			b.textContent = label;
			if (danger && value !== "cancel") b.className = "danger";
			b.addEventListener("click", () => { chosen = value; });
			bar.append(b);
		}
		const cancel = document.createElement("button");
		cancel.value = "cancel";
		cancel.textContent = "Abbrechen";
		cancel.formNoValidate = true;
		cancel.addEventListener("click", () => { chosen = null; });
		bar.append(cancel);
		form.append(bar);
		dlg.append(form);
		dlg.onclose = () => resolve(chosen ? { button: chosen, values: inputs.map((i) => i.value.trim()) } : null);
		dlg.showModal();
		(inputs[0] || bar.firstChild).focus();
	});
}

function toast(msg, undoBatch) {
	const t = $("#toast");
	t.replaceChildren(document.createTextNode(msg));
	if (undoBatch) {
		const b = document.createElement("button");
		b.textContent = "Rückgängig";
		b.addEventListener("click", () => undo(undoBatch));
		t.append(" ", b);
	}
	t.hidden = false;
	clearTimeout(toast.timer);
	toast.timer = setTimeout(() => { t.hidden = true; }, 8000);
}

function fail(err) {
	toast(err.message || String(err));
}

// ---------------------------------------------------------------- tree

const tree = $("#tree");

function treeItem(n) {
	const li = document.createElement("li");
	li.dataset.id = n.id;
	li.dataset.kind = n.kind;
	if (n.facet) li.dataset.facet = n.facet;
	if (n.tag) li.dataset.tag = n.tag;
	if (n.rev !== undefined) li.dataset.rev = n.rev;
	const row = document.createElement("div");
	row.className = "tn";
	const tog = document.createElement("span");
	tog.className = "tog";
	tog.textContent = n.children ? "▸" : "";
	const label = document.createElement("span");
	label.className = "tl";
	label.textContent = n.label;
	row.append(tog, label);
	if (n.count) {
		const c = document.createElement("span");
		c.className = "tc";
		c.textContent = n.count;
		row.append(c);
	}
	li.append(row);
	tog.addEventListener("click", (e) => { e.stopPropagation(); toggle(li); });
	row.addEventListener("click", () => open(n.id));
	if (n.kind === "tag" && n.facet !== "location") {
		row.draggable = true;
		row.addEventListener("dragstart", (e) => {
			e.dataTransfer.setData("application/x-oc-folder", JSON.stringify({ tag: n.tag, rev: n.rev, facet: n.facet, label: n.label }));
			e.dataTransfer.effectAllowed = "move";
		});
	}
	row.addEventListener("dragover", (e) => dragOver(e, li));
	row.addEventListener("dragleave", () => row.classList.remove("drop"));
	row.addEventListener("drop", (e) => drop(e, li));
	return li;
}

async function toggle(li, force) {
	if (li.dataset.id === "saved") { renderSaved(); return; }
	const open = force ?? !li.classList.contains("open");
	let ul = li.querySelector(":scope > ul");
	if (!open) {
		li.classList.remove("open");
		li.querySelector(".tog").textContent = "▸";
		return;
	}
	if (!ul) {
		ul = document.createElement("ul");
		li.append(ul);
	}
	try {
		const nodes = await api("/api/tree?node=" + encodeURIComponent(li.dataset.id));
		ul.replaceChildren(...nodes.map(treeItem));
		li.classList.add("open");
		li.querySelector(".tog").textContent = nodes.length ? "▾" : "";
	} catch (err) { fail(err); }
}

async function refreshTree() {
	for (const li of tree.querySelectorAll("li.open")) await toggle(li, true);
}

// ---------------------------------------------------------------- pane

const pane = $("#pane");
const list = $("#items");

async function open(id, keepSelection = false) {
	state.loading = true;
	try {
		const c = await api("/api/node?node=" + encodeURIComponent(id) + "&limit=200");
		state.node = c.node;
		state.items = c.items || [];
		state.total = c.total;
		if (!keepSelection) state.sel.clear();
		state.sub = c.sub || [];
		history.replaceState(null, "", "#" + encodeURIComponent(id));
		for (const el of tree.querySelectorAll(".tn.cur")) el.classList.remove("cur");
		const li = tree.querySelector(`li[data-id="${CSS.escape(id)}"] > .tn`);
		if (li) li.classList.add("cur");
		render();
	} catch (err) { fail(err); } finally { state.loading = false; }
}

async function more() {
	if (state.loading || state.items.length >= state.total || !state.node) return;
	state.loading = true;
	try {
		const c = await api("/api/node?node=" + encodeURIComponent(state.node.id) + "&limit=200&offset=" + state.items.length);
		state.items.push(...(c.items || []));
		render();
	} catch (err) { fail(err); } finally { state.loading = false; }
}

pane.addEventListener("scroll", () => {
	if (pane.scrollTop + pane.clientHeight > pane.scrollHeight - 400) more();
});

function isTagNode() {
	return state.node && state.node.kind === "tag";
}

function render() {
	const n = state.node;
	$("#nodetitle").textContent = n ? n.label : "";
	const link = $("#nodelink");
	link.hidden = !(n && n.href);
	if (n && n.href) link.href = n.href;
	$("#count").textContent = n ? `${state.total} Objekte${state.sel.size ? `, ${state.sel.size} ausgewählt` : ""}` : "";
	for (const b of document.querySelectorAll("[data-view]")) b.classList.toggle("on", b.dataset.view === state.view);
	const canChange = editing;
	$("#b-newfolder").disabled = !canChange || !n || !(n.kind === "tag" || n.kind === "facet" || n.kind === "collections") || n.facet === "location";
	$("#b-newfolder").textContent = n && n.kind === "collections" ? "Neue Sammlung" : "Neuer Ordner";
	$("#b-rename").disabled = !canChange || !n || !((n.kind === "tag" && n.facet !== "location") || n.kind === "collection");
	$("#b-remove").disabled = !canChange || !(isTagNode() || (n && n.kind === "collection")) || !state.sel.size;
	$("#b-savesearch").hidden = !(n && n.id.startsWith("q:"));
	$("#b-paste").disabled = !canChange || !isTagNode() || !state.clip;
	$("#b-store").disabled = !canChange || !state.sel.size;
	$("#b-withdraw").disabled = !canChange || !state.sel.size;

	const subs = $("#subs");
	subs.replaceChildren(...(state.sub || []).map((s) => {
		const b = document.createElement("button");
		b.className = "sub";
		b.textContent = "📁 " + s.label + (s.count ? ` (${s.count})` : "");
		b.addEventListener("click", () => open(s.id));
		return b;
	}));

	list.className = "items v-" + state.view;
	if (state.view === "table") renderTable();
	else list.replaceChildren(...state.items.map(state.view === "tiles" ? tile : row));
	if (!state.items.length) {
		const p = document.createElement("p");
		p.className = "meta";
		p.textContent = n && n.kind === "facet" ? "Ein Schlagwort links oder oben wählen." : "Hier liegt nichts.";
		list.append(p);
	}
	renderSession();
}

function badges(it) {
	const span = document.createElement("span");
	span.className = "tags";
	for (const t of it.tags || []) {
		if (state.node && t.id === state.node.tag) continue;
		if (t.facet === "location" && state.node && state.node.facet !== "location") continue;
		const b = document.createElement("a");
		b.className = "tag t-" + t.facet;
		b.textContent = t.label.replace(/^[^:]*: /, "");
		b.title = t.label;
		b.href = "#t:" + t.id;
		b.addEventListener("click", (e) => { e.preventDefault(); open("t:" + t.id); });
		span.append(b);
	}
	return span;
}

function select(it, e) {
	if (e.shiftKey && state.anchor !== null) {
		const ids = state.items.map((x) => x.id);
		const [a, b] = [ids.indexOf(state.anchor), ids.indexOf(it.id)].sort((x, y) => x - y);
		for (const id of ids.slice(a, b + 1)) state.sel.add(id);
	} else if (e.ctrlKey || e.metaKey) {
		state.sel.has(it.id) ? state.sel.delete(it.id) : state.sel.add(it.id);
		state.anchor = it.id;
	} else {
		state.sel.clear();
		state.sel.add(it.id);
		state.anchor = it.id;
	}
	render();
}

function draggable(el, it) {
	el.draggable = editing;
	el.addEventListener("dragstart", (e) => {
		if (!state.sel.has(it.id)) { state.sel.clear(); state.sel.add(it.id); }
		const items = state.items.filter((x) => state.sel.has(x.id));
		e.dataTransfer.setData("application/x-oc-items", JSON.stringify({
			items: items.map((x) => x.id), revs: items.map((x) => x.rev),
			from: isTagNode() ? state.node.tag : 0, facet: state.node && state.node.facet,
		}));
		e.dataTransfer.effectAllowed = "copyMove";
	});
}

function row(it) {
	const d = document.createElement("div");
	d.className = "it" + (state.sel.has(it.id) ? " sel" : "") + (state.clip && state.clip.mode === "cut" && state.clip.items.includes(it.id) ? " cut" : "");
	d.dataset.id = it.id;
	const t = document.createElement("span");
	t.className = "ti";
	if (it.type) {
		const b = document.createElement("span");
		b.className = "badge";
		b.textContent = it.type;
		t.append(b, " ");
	}
	t.append(it.title);
	const meta = document.createElement("span");
	meta.className = "im";
	meta.textContent = `A${it.id} · ${it.files > 1 ? it.files + " Dateien · " : ""}${size(it.size)}${it.stored ? " · objekte/" : ""}`;
	if (it.others) {
		const o = document.createElement("span");
		o.className = "others";
		o.textContent = ` · auch in ${it.others} weiteren`;
		meta.append(o);
	}
	const a = document.createElement("a");
	a.href = "/a/" + it.id;
	a.target = "_blank";
	a.className = "open";
	a.textContent = "↗";
	a.title = "Seite des Objekts";
	d.append(t, a, meta, badges(it));
	d.addEventListener("click", (e) => { if (e.target.tagName !== "A") select(it, e); });
	d.addEventListener("dblclick", () => rename(it));
	draggable(d, it);
	return d;
}

function tile(it) {
	const d = row(it);
	d.classList.add("tile");
	if (it.thumb) {
		const img = document.createElement("img");
		img.src = it.thumb;
		img.loading = "lazy";
		img.alt = "";
		d.prepend(img);
	}
	return d;
}

const cols = [["title", "Titel"], ["descr", "Beschreibung"], ["year", "Jahr"]];

function renderTable() {
	const table = document.createElement("table");
	const head = document.createElement("tr");
	for (const h of ["", "Nr.", ...cols.map((c) => c[1]), "Schlagworte"]) {
		const th = document.createElement("th");
		th.textContent = h;
		head.append(th);
	}
	table.append(head);
	for (const it of state.items) {
		const tr = document.createElement("tr");
		tr.className = state.sel.has(it.id) ? "sel" : "";
		const cb = document.createElement("td");
		const box = document.createElement("input");
		box.type = "checkbox";
		box.checked = state.sel.has(it.id);
		box.addEventListener("click", (e) => { e.stopPropagation(); select(it, { ctrlKey: true }); });
		cb.append(box);
		const nr = document.createElement("td");
		nr.textContent = "A" + it.id;
		tr.append(cb, nr);
		for (const [field] of cols) {
			const td = document.createElement("td");
			td.textContent = it[field];
			td.contentEditable = editing ? "plaintext-only" : "false";
			td.dataset.field = field;
			td.addEventListener("blur", () => {
				const v = td.textContent.trim();
				if (v !== (it[field] || "")) batch([{ op: "set", items: [it.id], revs: [it.rev], field, value: v }], `${field} von A${it.id}`);
			});
			td.addEventListener("keydown", (e) => {
				if (e.key === "Enter") { e.preventDefault(); td.blur(); }
				if (e.key === "Escape") { td.textContent = it[field]; td.blur(); }
			});
			td.addEventListener("paste", (e) => pasteColumn(e, it, field));
			tr.append(td);
		}
		const tags = document.createElement("td");
		tags.append(badges(it));
		tr.append(tags);
		draggable(tr, it);
		table.append(tr);
	}
	list.replaceChildren(table);
}

// pasteColumn fills a column of the selected rows, from this one down, with
// the lines of the clipboard (a list from a spreadsheet).
function pasteColumn(e, it, field) {
	const text = e.clipboardData.getData("text/plain");
	const lines = text.replace(/\r/g, "").split("\n").filter((l, i, a) => l !== "" || i < a.length - 1);
	if (lines.length < 2) return;
	e.preventDefault();
	let rows = state.items.filter((x) => state.sel.has(x.id));
	if (rows.length < 2) rows = state.items;
	const start = rows.findIndex((x) => x.id === it.id);
	rows = rows.slice(Math.max(start, 0), Math.max(start, 0) + lines.length);
	batch([{ op: "set", field, items: rows.map((x) => x.id), revs: rows.map((x) => x.rev), values: lines.slice(0, rows.length) }],
		`${rows.length}× ${field} eingefügt`);
}

function size(n) {
	if (n < 1024) return n + " B";
	const u = ["kB", "MB", "GB", "TB"];
	let v = n / 1024, i = 0;
	while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
	return (v < 10 ? v.toFixed(1) : Math.round(v)).toString().replace(".", ",") + " " + u[i];
}

// -------------------------------------------------------------- actions

async function batch(ops, label) {
	try {
		const res = await api("/api/batch", { ops });
		if (res.batch) {
			state.session.unshift({ batch: res.batch, label, at: new Date() });
			toast(label + " – erledigt.", res.batch);
		} else if (res.changes) {
			toast(label + " – erledigt.");
		}
		await open(state.node.id, true);
		await refreshTree();
	} catch (err) {
		fail(err);
		if (state.node) open(state.node.id, true);
	}
}

async function undo(b) {
	try {
		await api("/api/undo", { batch: b });
		state.session = state.session.filter((s) => s.batch !== b);
		toast("Rückgängig gemacht.");
		await open(state.node.id, true);
		await refreshTree();
	} catch (err) { fail(err); }
}

function renderSession() {
	const ul = $("#session");
	ul.replaceChildren(...state.session.slice(0, 30).map((s) => {
		const li = document.createElement("li");
		li.textContent = `${s.at.toLocaleTimeString("de-DE")} ${s.label} `;
		const b = document.createElement("button");
		b.textContent = "rückgängig";
		b.addEventListener("click", () => undo(s.batch));
		li.append(b);
		return li;
	}));
	$("#sessionbox").hidden = !state.session.length;
}

function selected() {
	const items = state.items.filter((x) => state.sel.has(x.id));
	return { items: items.map((x) => x.id), revs: items.map((x) => x.rev) };
}

async function rename(it) {
	if (!editing) return;
	const r = await ask({ title: "Umbenennen", fields: [{ label: "Titel", value: it.title }], buttons: [["ok", "Speichern"]] });
	if (r && r.values[0] && r.values[0] !== it.title) {
		batch([{ op: "set", field: "title", items: [it.id], revs: [it.rev], value: r.values[0] }], `A${it.id} umbenannt`);
	}
}

async function newFolder() {
	const n = state.node;
	if (n.kind === "collections") {
		const r = await ask({ title: "Neue Sammlung", fields: [{ label: "Name", placeholder: "Einstieg in die PDP-11" }], buttons: [["ok", "Anlegen"]] });
		if (r && r.values[0]) batch([{ op: "new-collection", name: r.values[0] }], `Sammlung ${r.values[0]} angelegt`);
		return;
	}
	const r = await ask({ title: "Neuer Ordner", text: `unter ${n.label}`, fields: [{ label: "Name" }], buttons: [["ok", "Anlegen"]] });
	if (r && r.values[0]) batch([{ op: "new-folder", to: n.tag || 0, facet: n.facet, name: r.values[0] }], `Ordner ${r.values[0]} angelegt`);
}

async function renameFolder() {
	const n = state.node;
	if (n.kind === "collection") {
		const cur = n.label.replace(/^Sammlung: /, "");
		const r = await ask({ title: "Sammlung umbenennen", fields: [{ label: "Name", value: cur }], buttons: [["ok", "Speichern"]] });
		if (r && r.values[0] && r.values[0] !== cur) batch([{ op: "rename-collection", tag: n.tag, revs: [n.rev], name: r.values[0] }], `Sammlung ${cur} umbenannt`);
		return;
	}
	const name = n.label.split(" › ").pop();
	const r = await ask({ title: "Ordner umbenennen", fields: [{ label: "Name", value: name }], buttons: [["ok", "Speichern"]] });
	if (r && r.values[0] && r.values[0] !== name) batch([{ op: "rename-folder", tag: n.tag, revs: [n.rev], name: r.values[0] }], `Ordner ${name} umbenannt`);
}

function removeHere() {
	if (!state.sel.size) return;
	const s = selected();
	if (state.node && state.node.kind === "collection") {
		batch([{ op: "uncollect", to: state.node.tag, items: s.items }], `${s.items.length} aus der Sammlung genommen`);
		return;
	}
	if (!isTagNode()) return;
	batch([{ op: "remove", from: state.node.tag, ...s }], `${s.items.length} aus ${state.node.label} entfernt`);
}

async function withdraw() {
	const s = selected();
	const r = await ask({ title: `${s.items.length} Objekte zurückziehen`, text: "Zurückgezogene Objekte bleiben mit einem Hinweis erreichbar und lassen sich wiederherstellen.",
		fields: [{ label: "Grund", placeholder: "Dublette von A…, Urheberrecht …" }], buttons: [["ok", "Zurückziehen"]], danger: true });
	if (r && r.values[0]) batch([{ op: "withdraw", reason: r.values[0], ...s }], `${s.items.length} zurückgezogen`);
}

async function store() {
	const s = selected();
	const r = await ask({ title: `${s.items.length} Objekte nach objekte/ verschieben`,
		text: "Die Dateien ziehen auf dem Speicher um, nach objekte/<Nummer>. Alte Links leiten weiter; Gesicherte Websites ziehen als Ganzes um.",
		buttons: [["ok", "Verschieben"]] });
	if (r) batch([{ op: "store", ...s }], `${s.items.length} nach objekte/ verschoben`);
}

function copy(mode) {
	if (!state.sel.size) return;
	const s = selected();
	state.clip = { mode, ...s, from: isTagNode() ? state.node.tag : 0 };
	toast(`${s.items.length} ${mode === "cut" ? "ausgeschnitten" : "kopiert"} – im Zielordner einfügen.`);
	render();
}

function paste() {
	if (!state.clip || !isTagNode()) return;
	const c = state.clip;
	const op = c.mode === "cut" ? "move" : "add";
	batch([{ op, items: c.items, revs: c.revs, from: c.from, to: state.node.tag }],
		`${c.items.length} ${op === "move" ? "verschoben" : "auch abgelegt"} in ${state.node.label}`);
	if (c.mode === "cut") state.clip = null;
}

// ---------------------------------------------------------- drag & drop

function dragOver(e, li) {
	if (!editing || (li.dataset.kind !== "tag" && li.dataset.kind !== "collection")) return;
	const types = e.dataTransfer.types;
	if (!types.includes("application/x-oc-items") && !types.includes("application/x-oc-folder")) return;
	e.preventDefault();
	e.dataTransfer.dropEffect = e.altKey || e.ctrlKey ? "copy" : "move";
	li.querySelector(".tn").classList.add("drop");
}

async function drop(e, li) {
	e.preventDefault();
	li.querySelector(".tn").classList.remove("drop");
	const to = Number(li.dataset.tag);
	const label = li.querySelector(".tl").textContent;
	const items = e.dataTransfer.getData("application/x-oc-items");
	if (items && li.dataset.kind === "collection") {
		const d = JSON.parse(items);
		batch([{ op: "collect", items: d.items, to }], `${d.items.length} in die Sammlung ${label}`);
		return;
	}
	if (items) {
		const d = JSON.parse(items);
		// within one facet a drop moves, across facets or with Alt/Ctrl it adds
		const move = !(e.altKey || e.ctrlKey) && d.from && d.facet === li.dataset.facet;
		batch([{ op: move ? "move" : "add", items: d.items, revs: d.revs, from: d.from, to }],
			`${d.items.length} ${move ? "verschoben nach" : "auch abgelegt in"} ${label}`);
		return;
	}
	const folder = e.dataTransfer.getData("application/x-oc-folder");
	if (folder) {
		const f = JSON.parse(folder);
		if (f.tag === to) return;
		const r = await ask({ title: `${f.label} → ${label}`, text: "Unter den Ordner verschieben, oder beide zu einem zusammenlegen?",
			buttons: [["move", "Verschieben"], ["merge", "Zusammenlegen"]] });
		if (!r) return;
		batch([{ op: r.button === "move" ? "move-folder" : "merge-folders", tag: f.tag, revs: [f.rev], to }],
			`${f.label} ${r.button === "move" ? "verschoben unter" : "zusammengelegt mit"} ${label}`);
	}
}

// files from the desktop become items in the open node
pane.addEventListener("dragover", (e) => {
	if (!editing || !e.dataTransfer.types.includes("Files")) return;
	e.preventDefault();
	pane.classList.add("dropfiles");
});
pane.addEventListener("dragleave", () => pane.classList.remove("dropfiles"));
pane.addEventListener("drop", async (e) => {
	if (!e.dataTransfer.files.length) return;
	e.preventDefault();
	pane.classList.remove("dropfiles");
	const n = state.node;
	if (!n || n.kind !== "tag" || n.facet === "location") {
		toast("Dateien bitte auf ein Schlagwort ziehen, nicht in den alten Ordnerbaum.");
		return;
	}
	const files = [...e.dataTransfer.files];
	const r = await ask({ title: `${files.length} ${files.length === 1 ? "Datei" : "Dateien"} ablegen`,
		text: `Jede Datei wird ein neues Objekt unter ${n.label}, abgelegt in objekte/.`, buttons: [["ok", "Hochladen"]] });
	if (!r) return;
	const fd = new FormData();
	for (const f of files) fd.append("file", f, f.name);
	const xhr = new XMLHttpRequest();
	xhr.open("POST", "/api/upload?node=" + encodeURIComponent(n.id));
	xhr.setRequestHeader("X-CSRF-Token", csrf);
	xhr.upload.onprogress = (p) => { if (p.lengthComputable) toast(`Hochladen … ${Math.round(100 * p.loaded / p.total)} %`); };
	xhr.onload = () => {
		const res = JSON.parse(xhr.responseText || "{}");
		if (xhr.status !== 200) { toast(res.error || "Hochladen fehlgeschlagen"); return; }
		toast(`${res.items.length} neue Objekte unter ${n.label}.`);
		open(n.id, true);
	};
	xhr.onerror = () => toast("Hochladen fehlgeschlagen");
	xhr.send(fd);
});

// --------------------------------------------------------------- wiring

for (const b of document.querySelectorAll("[data-view]")) {
	b.addEventListener("click", () => { state.view = b.dataset.view; localSet("view", state.view); render(); });
}
$("#b-newfolder").addEventListener("click", newFolder);
$("#b-rename").addEventListener("click", renameFolder);
$("#b-remove").addEventListener("click", removeHere);
$("#b-paste").addEventListener("click", paste);
$("#b-store").addEventListener("click", store);
$("#b-withdraw").addEventListener("click", withdraw);
$("#q").addEventListener("submit", (e) => {
	e.preventDefault();
	const v = $("#q input").value.trim();
	if (v) open("q:" + v);
});

document.addEventListener("keydown", (e) => {
	if (dlg.open || e.target.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName)) return;
	const mod = e.ctrlKey || e.metaKey;
	if (mod && e.key === "a") { e.preventDefault(); for (const it of state.items) state.sel.add(it.id); render(); }
	else if (mod && e.key === "x" && editing) { e.preventDefault(); copy("cut"); }
	else if (mod && e.key === "c" && editing) { e.preventDefault(); copy("copy"); }
	else if (mod && e.key === "v" && editing) { e.preventDefault(); paste(); }
	else if (mod && e.key === "z" && editing && state.session.length) { e.preventDefault(); undo(state.session[0].batch); }
	else if ((e.key === "Delete" || e.key === "Backspace") && editing) { e.preventDefault(); removeHere(); }
	else if (e.key === "F2" && editing && state.sel.size === 1) {
		e.preventDefault();
		rename(state.items.find((x) => state.sel.has(x.id)));
	} else if (e.key === "Escape") { state.sel.clear(); render(); }
});

// live updates: reload what someone else changed
const es = new EventSource("/api/events");
es.onmessage = (m) => {
	const ev = JSON.parse(m.data);
	if (!state.node) return;
	const here = new Set(state.items.map((x) => "item:" + x.id));
	if (state.node.tag) here.add("tag:" + state.node.tag);
	if (ev.targets.some((t) => here.has(t))) open(state.node.id, true);
	if (ev.targets.some((t) => t.startsWith("tag:"))) refreshTree();
};

// saved searches live in this browser only
function savedSearches() {
	try { return JSON.parse(localGet("searches") || "[]"); } catch { return []; }
}

function renderSaved() {
	let li = tree.querySelector('li[data-id="saved"]');
	const list = savedSearches();
	if (!li) {
		li = treeItem({ id: "saved", label: "Gespeicherte Suchen", kind: "saved", children: true });
		li.querySelector(".tn").onclick = () => toggle(li);
		li.querySelector(".tog").onclick = (e) => { e.stopPropagation(); li.classList.toggle("open"); };
		tree.append(li);
	}
	let ul = li.querySelector(":scope > ul");
	if (!ul) { ul = document.createElement("ul"); li.append(ul); }
	ul.replaceChildren(...list.map((q) => treeItem({ id: "q:" + q, label: q, kind: "view" })));
	li.classList.add("open");
	li.querySelector(".tog").textContent = list.length ? "▾" : "";
}

$("#b-savesearch").addEventListener("click", () => {
	const q = state.node.id.slice(2);
	const list = savedSearches().filter((x) => x !== q);
	list.unshift(q);
	localSet("searches", JSON.stringify(list.slice(0, 30)));
	renderSaved();
	toast(`Suche „${q}“ gemerkt.`);
});

async function start() {
	const nodes = await api("/api/tree?node=root");
	tree.replaceChildren(...nodes.map(treeItem));
	renderSaved();
	const want = decodeURIComponent(location.hash.slice(1));
	open(want || "f:machine");
	for (const li of tree.querySelectorAll(":scope > li")) {
		if (li.dataset.id === "f:machine") toggle(li, true);
	}
}
start().catch(fail);
