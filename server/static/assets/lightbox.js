/* lightbox for the contact sheets that
   bin/generate-pages.py writes. Works on the existing markup
   (.sheet .tile a > img + figcaption); without JavaScript the tiles
   simply link to the files as before. PDF tiles (.doc) and folder covers
   (.sub) are plain links and stay out of it.

   Keys: left/right, Home/End, Esc. Swipe on touch screens. The open
   picture is kept in the URL (#bild=name), so Back closes the viewer
   and a link to a single picture can be passed on. */

(function () {
	"use strict";

	// formats a browser can show; anything else gets the thumbnail and a link
	var SHOWABLE = /\.(jpe?g|png|gif|bmp|webp|svg)$/i;
	var tiles = Array.prototype.slice.call(document.querySelectorAll(".sheet .tile:not(.doc):not(.sub) > a"));
	if (!tiles.length) return;

	var items = tiles.map(function (a) {
		var img = a.querySelector("img");
		var cap = a.parentNode.querySelector("figcaption");
		var name = a.getAttribute("title") || decodeURIComponent(a.getAttribute("href"));
		return {
			link: a,
			href: a.href,
			thumb: img ? img.src : "",
			name: name,
			caption: cap && cap.textContent !== name ? cap.textContent : "",
			showable: SHOWABLE.test(name)
		};
	});

	function el(tag, cls, text) {
		var n = document.createElement(tag);
		if (cls) n.className = cls;
		if (text) n.textContent = text;
		return n;
	}

	var box = el("div", "lb");
	box.hidden = true;
	box.setAttribute("role", "dialog");
	box.setAttribute("aria-modal", "true");
	box.setAttribute("aria-label", "Bildbetrachter");

	var bar = el("div", "lb-bar");
	var count = el("span", "lb-count");
	var orig = el("a", "", "Original öffnen");
	var close = el("button", "lb-close", "Schließen ✕");
	close.type = "button";
	bar.append(count, el("span", "sp"), orig, close);

	var stage = el("div", "lb-stage");
	var picture = el("img");
	picture.alt = "";
	var msg = el("p", "lb-msg");
	var prev = el("button", "lb-prev", "‹");
	var next = el("button", "lb-next", "›");
	prev.type = next.type = "button";
	prev.setAttribute("aria-label", "Vorheriges Bild");
	next.setAttribute("aria-label", "Nächstes Bild");
	stage.append(prev, picture, msg, next);

	var cap = el("div", "lb-cap");
	box.append(bar, stage, cap);
	document.body.appendChild(box);

	var cur = -1, pushed = false, opener = null;

	/* Small pictures (screenshots, icons) are blown up by whole multiples
	   with hard pixels instead of being shown as a postage stamp. */
	function fit() {
		picture.style.width = picture.style.height = "";
		picture.classList.remove("pixel");
		var w = picture.naturalWidth, h = picture.naturalHeight;
		if (!w || !h) return;
		var sw = stage.clientWidth - (window.innerWidth > 544 ? 112 : 0);
		var sh = stage.clientHeight;
		var k = Math.min(Math.floor(sw / w), Math.floor(sh / h), 8);
		if (k >= 2) {
			picture.style.width = (w * k) + "px";
			picture.style.height = (h * k) + "px";
			picture.classList.add("pixel");
		}
	}

	function show(i) {
		cur = (i + items.length) % items.length;
		var it = items[cur];
		count.textContent = (cur + 1) + " / " + items.length;
		orig.href = it.href;
		orig.textContent = it.showable ? "Original öffnen" : "Datei herunterladen";
		msg.textContent = "";
		picture.hidden = false;
		picture.src = it.showable ? it.href : it.thumb;
		picture.alt = it.name;
		if (!it.showable) {
			msg.textContent = "";
		}
		cap.textContent = "";
		if (it.caption) cap.append(el("div", "", it.caption));
		var fn = el("div", "fn", it.name + (it.showable ? "" :
			" – dieses Format kann der Browser nicht anzeigen, hier nur die Vorschau"));
		cap.append(fn);
		prev.hidden = next.hidden = items.length < 2;
		// warm the cache for the neighbours
		[cur - 1, cur + 1].forEach(function (j) {
			var n = items[(j + items.length) % items.length];
			if (n.showable) new Image().src = n.href;
		});
	}

	picture.addEventListener("load", fit);
	picture.addEventListener("error", function () {
		var it = items[cur];
		if (it && picture.src !== it.thumb && it.thumb) {
			picture.src = it.thumb;         // damaged original: fall back
		} else {
			picture.hidden = true;
			msg.textContent = "Das Bild lässt sich nicht anzeigen.";
		}
	});
	window.addEventListener("resize", function () { if (!box.hidden) fit(); });

	function hashFor(i) {
		return "#bild=" + encodeURIComponent(items[i].name);
	}

	function indexFromHash() {
		var m = /^#bild=(.*)$/.exec(location.hash);
		if (!m) return -1;
		var name = decodeURIComponent(m[1]);
		for (var i = 0; i < items.length; i++) {
			if (items[i].name === name) return i;
		}
		return -1;
	}

	function open(i, fromHistory) {
		if (box.hidden) {
			opener = document.activeElement;
			box.hidden = false;
			document.body.classList.add("lb-open");
		}
		show(i);
		if (!fromHistory) {
			if (pushed) {
				history.replaceState({ lb: true }, "", hashFor(cur));
			} else {
				history.pushState({ lb: true }, "", hashFor(cur));
				pushed = true;
			}
		}
		close.focus();
	}

	function hide(fromHistory) {
		if (box.hidden) return;
		box.hidden = true;
		document.body.classList.remove("lb-open");
		picture.removeAttribute("src");
		if (!fromHistory) {
			if (pushed) {
				pushed = false;
				history.back();
			} else {
				history.replaceState(null, "", location.pathname + location.search);
			}
		}
		pushed = false;
		var back = items[cur] ? items[cur].link : opener;
		if (back) back.focus();
	}

	function step(d) { open(cur + d); }

	tiles.forEach(function (a, i) {
		a.addEventListener("click", function (ev) {
			if (ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
			ev.preventDefault();
			open(i);
		});
	});

	prev.addEventListener("click", function () { step(-1); });
	next.addEventListener("click", function () { step(1); });
	close.addEventListener("click", function () { hide(); });
	stage.addEventListener("click", function (ev) {
		if (ev.target === stage) hide();
	});

	document.addEventListener("keydown", function (ev) {
		if (box.hidden) return;
		switch (ev.key) {
		case "Escape": hide(); break;
		case "ArrowLeft": step(-1); break;
		case "ArrowRight": case " ": step(1); break;
		case "Home": open(0); break;
		case "End": open(items.length - 1); break;
		case "Tab":
			// keep the focus inside the viewer
			var f = [close, orig, prev, next].filter(function (n) { return !n.hidden; });
			var k = f.indexOf(document.activeElement);
			var to = ev.shiftKey ? (k <= 0 ? f.length - 1 : k - 1) : (k + 1) % f.length;
			f[to].focus();
			break;
		default: return;
		}
		ev.preventDefault();
	});

	var x0 = null, y0 = null;
	stage.addEventListener("touchstart", function (ev) {
		if (ev.touches.length !== 1) { x0 = null; return; }
		x0 = ev.touches[0].clientX;
		y0 = ev.touches[0].clientY;
	}, { passive: true });
	stage.addEventListener("touchend", function (ev) {
		if (x0 === null) return;
		var dx = ev.changedTouches[0].clientX - x0;
		var dy = ev.changedTouches[0].clientY - y0;
		x0 = null;
		if (Math.abs(dx) > 50 && Math.abs(dx) > 1.5 * Math.abs(dy)) step(dx < 0 ? 1 : -1);
	});

	window.addEventListener("popstate", function () {
		var i = indexFromHash();
		if (i >= 0) {
			pushed = true;
			open(i, true);
		} else {
			hide(true);
		}
	});

	// a link straight to one picture
	var start = indexFromHash();
	if (start >= 0) {
		history.replaceState({ lb: true }, "", hashFor(start));
		open(start, true);
	}
})();
