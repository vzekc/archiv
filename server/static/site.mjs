// Autocompletion of tags in the edit forms: inputs with data-suggest fill
// the datalist they refer to from /api/schlagworte.
const timers = new WeakMap();
for (const input of document.querySelectorAll("input[data-suggest]")) {
	input.addEventListener("input", () => {
		clearTimeout(timers.get(input));
		const q = input.value.replace(/^[^:]*:\s*/, "").split("›").pop().trim();
		if (q.length < 2) return;
		timers.set(input, setTimeout(async () => {
			const res = await fetch("/api/schlagworte?q=" + encodeURIComponent(q));
			if (!res.ok) return;
			const list = document.getElementById(input.getAttribute("list"));
			list.replaceChildren(...(await res.json() || []).map((t) => {
				const o = document.createElement("option");
				o.value = t.label;
				return o;
			}));
		}, 200));
	});
}
