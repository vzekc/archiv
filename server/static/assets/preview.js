/* large preview of a PDF's first page on hover,
   for the contact sheets that bin/generate-pages.py writes. Tiles carry the
   picture in data-preview (html/previews/<path>.jpg, 1200 px, made by
   bin/make-thumbs.py); the thumbnail is shown enlarged at once and replaced
   by the sharp version as soon as it has loaded.

   Only where there is a real mouse: on touch screens a tap opens the PDF as
   before. The panel ignores the pointer, so it never gets in the way. */

(function () {
	"use strict";

	if (!window.matchMedia("(hover: hover) and (pointer: fine)").matches) return;
	var tiles = document.querySelectorAll(".sheet .tile[data-preview]");
	if (!tiles.length) return;

	var DELAY = 250;        // ms before the panel shows; passing over does nothing
	var GAP = 12;           // px between tile and panel
	var MARGIN = 16;        // px kept free at the window edge

	var panel = document.createElement("div");
	panel.className = "pv";
	panel.hidden = true;
	var img = document.createElement("img");
	img.alt = "";
	panel.appendChild(img);
	document.body.appendChild(panel);

	var current = null, timer = null, loader = null;

	function place(tile) {
		var r = tile.getBoundingClientRect();
		var vw = document.documentElement.clientWidth, vh = window.innerHeight;
		var ratio = img.naturalWidth && img.naturalHeight
			? img.naturalWidth / img.naturalHeight : 0.72;
		var h = Math.min(vh - 2 * MARGIN, 1200);
		var w = h * ratio;
		if (w > vw - 2 * MARGIN) { w = vw - 2 * MARGIN; h = w / ratio; }
		var left;
		if (r.right + GAP + w <= vw - MARGIN) left = r.right + GAP;
		else if (r.left - GAP - w >= MARGIN) left = r.left - GAP - w;
		else left = (vw - w) / 2;
		panel.style.width = w + "px";
		panel.style.height = h + "px";
		panel.style.left = left + "px";
		panel.style.top = (vh - h) / 2 + "px";
	}

	function show(tile) {
		var thumb = tile.querySelector("img");
		img.src = thumb ? thumb.currentSrc || thumb.src : "";
		place(tile);
		panel.hidden = false;
		if (loader && loader.complete && loader.naturalWidth) {
			img.src = loader.src;
			place(tile);
		}
	}

	function enter(tile) {
		if (current === tile) return;
		hide();
		current = tile;
		loader = new Image();
		loader.onload = function () {
			if (current !== tile || panel.hidden) return;
			img.src = loader.src;
			place(tile);
		};
		loader.src = tile.getAttribute("data-preview");
		timer = setTimeout(function () { show(tile); }, DELAY);
	}

	function hide() {
		clearTimeout(timer);
		current = null;
		panel.hidden = true;
	}

	Array.prototype.forEach.call(tiles, function (tile) {
		tile.addEventListener("mouseenter", function () { enter(tile); });
		tile.addEventListener("mouseleave", hide);
	});
	window.addEventListener("scroll", hide, { passive: true });
	window.addEventListener("blur", hide);
	document.addEventListener("keydown", function (ev) { if (ev.key === "Escape") hide(); });
})();
