package archiv

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/anthropics/anthropic-sdk-go/v2"
)

// classify tags the existing archive: items without a machine or kind tag
// (or with only a guess from the folder) are put before the AI folder by
// folder, with tools over the catalog and the text of the documents. What
// it finds becomes proposals of the user "ai:<run>", one batch per folder,
// for the moderators to accept or reject in /moderation; nothing is
// applied directly. Two steps need no AI: files with the same bytes become
// "same-as" relations, and obvious parts of one set (disk1, disk2 ...)
// are proposed for merging.
//
// With -pages it proposes, also without the AI, to gather the page scans
// of a document lying in a folder of their own into one item, and so the
// image of a cover tape or disk with the pictures of its inlay.
//
//	archiv classify [-folder path] [-limit folders] [-relations] [-pages]

const classifyBatch = 60

func classifyCmd(args []string) {
	fs := flag.NewFlagSet("classify", flag.ExitOnError)
	folder := fs.String("folder", "", "only items below this folder of the old tree")
	limit := fs.Int("limit", 10, "at most this many folders per run")
	relations := fs.Bool("relations", false, "propose same-as relations and merges of sets, without the AI")
	pages := fs.Bool("pages", false, "propose to gather folders of page scans into one item each, without the AI")
	titles := fs.Bool("titles", false, "propose titles for the items called by their file name, read from their text (AI)")
	itemRange := fs.String("items", "", "propose titles and kinds for these items from their file names (AI), e.g. 94795-94841")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		log.Fatal(err)
	}
	a, err := newApp(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	run := "ai:" + newBatchID()
	if *itemRange != "" {
		var lo, hi int64
		if _, err := fmt.Sscanf(*itemRange, "%d-%d", &lo, &hi); err != nil {
			if _, err := fmt.Sscanf(*itemRange, "%d", &lo); err != nil {
				log.Fatal("classify: -items from-to")
			}
			hi = lo
		}
		var ids []int64
		for id := lo; id <= hi; id++ {
			ids = append(ids, id)
		}
		n, err := a.proposeTitlesKinds(ctx, run, ids)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("classify: titles and kinds proposed for %d items", n)
		return
	}
	if *titles {
		if !a.ai.enabled() {
			log.Fatal("classify: no API key (api_key_file)")
		}
		n, err := a.proposeTitles(ctx, run, *folder, max(*limit, 1)*classifyBatch)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("classify: %d titles proposed", n)
		return
	}
	if *pages {
		// the disks with their unpacked folders first: they hold what the
		// others would gather piecemeal
		d, err := a.proposeDisks(run, *folder)
		if err != nil {
			log.Fatal(err)
		}
		n, err := a.proposePages(run, *folder)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("classify: %d disks with their contents, %d folders of pages proposed", d, n)
		return
	}
	if *relations {
		n, err := a.proposeRelations(run, *folder)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("classify: %d relations and merges proposed", n)
		return
	}
	if !a.ai.enabled() {
		log.Fatal("classify: no API key (api_key_file)")
	}
	folders, err := a.foldersToClassify(*folder, *limit)
	if err != nil {
		log.Fatal(err)
	}
	total := 0
	for _, f := range folders {
		if ctx.Err() != nil {
			break
		}
		n, err := a.classifyFolder(ctx, run, f)
		if err != nil {
			log.Printf("classify %s: %v", f, err)
			continue
		}
		total += n
	}
	log.Printf("classify: %d proposals in %d folders, run %s", total, len(folders), run)
}

// untaggedWhere selects live items whose kind is at most a guess from the
// folder, or that belong under a machine (their kind is a section there)
// without having one, and that have no proposals waiting. Magazines,
// meeting photos and the like need no machine.
const untaggedWhere = needsTagsWhere + `
    AND NOT EXISTS (SELECT 1 FROM revisions r WHERE r.target = 'item:' || i.id AND r.state = 'proposed')`

// needsTagsWhere: the items "Unsortiert" shows.
const needsTagsWhere = `i.state = 'live' AND i.root IS NULL
    AND (NOT EXISTS (SELECT 1 FROM item_tags it JOIN tags t ON t.id = it.tag WHERE it.item = i.id AND t.facet = 'kind' AND it.added_by != 'import:weak')
      OR (EXISTS (SELECT 1 FROM item_tags it JOIN tags t ON t.id = it.tag WHERE it.item = i.id AND t.facet = 'kind' AND t.section = 1)
          AND NOT EXISTS (SELECT 1 FROM item_tags it JOIN tags t ON t.id = it.tag WHERE it.item = i.id AND t.facet IN ('machine', 'publication', 'event'))))`

// foldersToClassify are the folders holding the most such items.
func (a *App) foldersToClassify(below string, limit int) ([]string, error) {
	lo, hi := pathRange(below)
	rows, err := a.cat.Query(`SELECT rtrim(rtrim(f.path, replace(f.path, '/', '')), '/') AS dir, COUNT(*) n
	    FROM items i JOIN item_files f ON f.item = i.id
	    WHERE `+untaggedWhere+` AND f.path >= ? AND f.path < ? AND f.path NOT LIKE 'objekte/%'
	    GROUP BY dir ORDER BY n DESC LIMIT ?`, lo, hi, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		var n int
		rows.Scan(&d, &n)
		out = append(out, d)
	}
	return out, nil
}

type classifyItem struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Name  string `json:"file"`
	Tags  string `json:"tags"`
	Text  string `json:"text,omitempty"`
}

type classifyAnswer struct {
	Items []struct {
		ID         int64   `json:"id"`
		Machine    string  `json:"machine"`
		NewMachine bool    `json:"new_machine"`
		Kind       string  `json:"kind"`
		System     string  `json:"system"`
		Language   string  `json:"language"`
		Confidence float64 `json:"confidence"`
		Note       string  `json:"note"`
	} `json:"items"`
}

const classifyPrompt = `Du ordnest den Bestand im {archive} ({site}) mit Schlagworten. Der Bestand liegt noch in einem gewachsenen Ordnerbaum; jedes Stück ("Objekt") soll den Rechner bekommen, zu dem es gehört, und seine Art.

Schlagworte:
- machine: Hersteller > Serie > Modell, z. B. "DEC > PDP-11 > DEC Pro 380" oder "Commodore > C64" (eine Ebene darf fehlen). Nimm das genaueste vorhandene Schlagwort; was für eine ganze Serie gilt, bekommt die Serie. Gibt es den Rechner noch nicht, schlage ihn unter dem vorhandenen Hersteller (oder der Serie) vor und setze new_machine. Sieh mit den Werkzeugen nach, bevor du entscheidest; erfinde keine Hersteller, die es im Baum schon unter anderem Namen gibt.
- kind: genau eine dieser Arten: {kinds}. Leer, wenn es keine davon ist.
- system: Betriebssystem, wo es eindeutig ist (CP/M, MS-DOS, RSTS/E ...), sonst leer.
- language: de, en, nl ..., die Sprache eines Dokuments, sonst leer.

Der Ordner, aus dem die Objekte kommen, und seine Beschreibungen sind ein starker Hinweis, aber nicht immer richtig. Lass Felder leer, statt zu raten; confidence zwischen 0 und 1. In note ein kurzer Satz, wenn etwas auffällt (Dublette, falscher Ordner, mehrere Teile eines Satzes).`

func (a *App) classifySchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":          map[string]any{"type": "integer"},
						"machine":     map[string]any{"type": "string"},
						"new_machine": map[string]any{"type": "boolean"},
						"kind":        map[string]any{"type": "string"},
						"system":      map[string]any{"type": "string"},
						"language":    map[string]any{"type": "string"},
						"confidence":  map[string]any{"type": "number"},
						"note":        map[string]any{"type": "string"},
					},
					"required":             []string{"id", "machine", "new_machine", "kind", "system", "language", "confidence", "note"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"items"},
		"additionalProperties": false,
	}
}

var classifyTools = []anthropic.ToolUnionParam{
	toolParam("list_tag", "Die Schlagworte unter einem Schlagwort, mit der Zahl ihrer Objekte. Ohne path: die oberste Ebene der Facette.",
		map[string]any{"facet": map[string]any{"type": "string", "enum": []string{"machine", "system", "publication"}},
			"path": map[string]any{"type": "string", "description": "z. B. \"DEC > PDP-11\""}}, "facet"),
	toolParam("search_tags", "Schlagworte, deren Name den Text enthält, mit ihrem Pfad.",
		map[string]any{"q": map[string]any{"type": "string"}}, "q"),
	toolParam("search_items", "Objekte zu einem Suchbegriff, mit ihren Schlagworten: so sieht man, wie Ähnliches eingeordnet ist.",
		map[string]any{"q": map[string]any{"type": "string"}}, "q"),
	toolParam("item_text", "Der Anfang des Textes eines Dokuments (aus PDF-Textlage oder OCR).",
		map[string]any{"id": map[string]any{"type": "integer"}}, "id"),
}

func (a *App) runClassifyTool(name string, input json.RawMessage) (string, bool) {
	var in struct {
		Facet, Path, Q string
		ID             int64
	}
	json.Unmarshal(input, &in)
	var b strings.Builder
	switch name {
	case "list_tag":
		var parent int64
		if in.Path != "" {
			id, err := a.resolveTag(facetLabels[in.Facet] + ": " + strings.ReplaceAll(in.Path, " > ", " › "))
			if err != nil {
				return "Das Schlagwort " + in.Path + " gibt es nicht.", true
			}
			parent = id
		}
		q := "parent IS NULL AND facet = ?"
		var arg any = in.Facet
		if parent != 0 {
			q, arg = "parent = ?", parent
		}
		for _, n := range a.tagNodes(q, arg) {
			fmt.Fprintf(&b, "%s (%d)%s\n", n.Label, n.Count, map[bool]string{true: " …", false: ""}[n.Children])
		}
	case "search_tags":
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(in.Q) + "%"
		rows, err := a.cat.Query(`SELECT id FROM tags WHERE facet NOT IN ('location', 'group') AND merged_into IS NULL AND name LIKE ? ESCAPE '\' LIMIT 30`, like)
		if err != nil {
			return err.Error(), true
		}
		var ids []int64
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			if t := a.tagRef(id); t != nil {
				fmt.Fprintf(&b, "%s: %s\n", t.Facet, strings.ReplaceAll(t.Path, " › ", " > "))
			}
		}
	case "search_items":
		m := ftsQuery(in.Q)
		if m == "" {
			return "leere Suche", true
		}
		for _, it := range a.browserItems("i.state = 'live' AND i.id IN (SELECT rowid FROM items_fts WHERE items_fts MATCH ? LIMIT 15)", []any{m}, 0) {
			var tags []string
			for _, t := range it.Tags {
				if t.Facet != "location" {
					tags = append(tags, strings.ReplaceAll(t.Label, " › ", " > "))
				}
			}
			fmt.Fprintf(&b, "A%d %s (%s) – %s\n", it.ID, it.Title, it.Path, strings.Join(tags, "; "))
		}
	case "item_text":
		var p string
		a.cat.QueryRow("SELECT path FROM item_files WHERE item = ? ORDER BY ord LIMIT 1", in.ID).Scan(&p)
		if pdb := a.pages(); pdb != nil && p != "" {
			rows, err := pdb.Query("SELECT body FROM pages WHERE path = ? ORDER BY page LIMIT 3", p)
			if err == nil {
				for rows.Next() {
					var t string
					rows.Scan(&t)
					b.WriteString(t)
					b.WriteString("\n")
				}
				rows.Close()
			}
		}
		if b.Len() == 0 {
			return "Für dieses Objekt gibt es keinen Text.", false
		}
		return limitBytes(b.String(), 4000), false
	default:
		return "unbekanntes Werkzeug", true
	}
	if b.Len() == 0 {
		return "nichts gefunden", false
	}
	return b.String(), false
}

// classifyFolder asks the AI about the untagged items of one folder and
// stores its answer as proposals.
func (a *App) classifyFolder(ctx context.Context, run, folder string) (int, error) {
	lo, hi := pathRange(folder)
	rows, err := a.cat.Query(`SELECT i.id, i.title, f.path FROM items i JOIN item_files f ON f.item = i.id
	    WHERE `+untaggedWhere+` AND f.path >= ? AND f.path < ? AND instr(substr(f.path, ?), '/') = 0
	    ORDER BY f.path LIMIT ?`, lo, hi, len(lo)+1, classifyBatch)
	if err != nil {
		return 0, err
	}
	var items []classifyItem
	for rows.Next() {
		var it classifyItem
		var p string
		rows.Scan(&it.ID, &it.Title, &p)
		it.Name = path.Base(p)
		items = append(items, it)
	}
	rows.Close()
	if len(items) == 0 {
		return 0, nil
	}
	for i := range items {
		var tags []string
		for _, t := range a.browserItems("i.id = ?", []any{items[i].ID}, 0)[0].Tags {
			if t.Facet != "location" {
				tags = append(tags, t.Label)
			}
		}
		items[i].Tags = strings.Join(tags, "; ")
		if len(items) <= 20 {
			if out, _ := a.runClassifyTool("item_text", json.RawMessage(fmt.Sprintf(`{"id": %d}`, items[i].ID))); !strings.HasPrefix(out, "Für dieses") {
				items[i].Text = limitBytes(out, 600)
			}
		}
	}
	kinds := a.kindChoices()
	var top []string
	idx := a.index()
	for _, id := range idx.roots["machine"] {
		if len(idx.tags[id].Items) > 0 || len(idx.tags[id].Children) > 0 {
			top = append(top, idx.tags[id].Name)
		}
	}
	sort.Slice(top, func(i, j int) bool { return naturalLess(top[i], top[j]) })
	ctxText := fmt.Sprintf("Ordner: %s (%s)\nBeschreibung des Ordners: %s\nVorhandene Hersteller: %s\n\nObjekte:\n",
		folder, a.cat.folderLabel(folder), a.folderDescr(folder), strings.Join(top, ", "))
	itemsJSON, _ := json.MarshalIndent(items, "", " ")
	system := strings.ReplaceAll(promptText(classifyPrompt), "{kinds}", strings.Join(kinds, ", "))
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.cfg.Model),
		MaxTokens: 32000,
		System:    []anthropic.TextBlockParam{{Text: system, CacheControl: anthropic.NewCacheControlEphemeralParam()}},
		Tools:     classifyTools,
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortHigh,
			Format: anthropic.JSONOutputFormatParam{Schema: a.classifySchema()},
		},
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(
			anthropic.NewTextBlock(ctxText+string(itemsJSON)),
			anthropic.NewTextBlock("Schlage für jedes Objekt die Schlagworte vor."))},
	}
	var ans classifyAnswer
	for round := 0; ; round++ {
		if round >= maxRounds-1 {
			params.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
		}
		resp, err := a.ai.send(ctx, params)
		if err != nil {
			return 0, err
		}
		if resp.StopReason == anthropic.StopReasonRefusal || resp.StopReason == anthropic.StopReasonMaxTokens {
			return 0, fmt.Errorf("stop reason %s", resp.StopReason)
		}
		params.Messages = append(params.Messages, resp.ToParam())
		var text string
		var results []anthropic.ContentBlockParamUnion
		for _, b := range resp.Content {
			switch v := b.AsAny().(type) {
			case anthropic.TextBlock:
				text += v.Text
			case anthropic.ToolUseBlock:
				out, isErr := a.runClassifyTool(v.Name, json.RawMessage(v.JSON.Input.Raw()))
				results = append(results, anthropic.NewToolResultBlock(v.ID, out, isErr))
			}
		}
		if resp.StopReason == anthropic.StopReasonToolUse {
			params.Messages = append(params.Messages, anthropic.NewUserMessage(results...))
			continue
		}
		if err := json.Unmarshal([]byte(text), &ans); err != nil {
			return 0, fmt.Errorf("answer: %v", err)
		}
		break
	}
	changes := a.classifyChanges(run, items, ans)
	if len(changes) == 0 {
		return 0, nil
	}
	if _, err := a.cat.Propose(run, changes); err != nil {
		return 0, err
	}
	logf("classify %s: %d proposals for %d items", folder, len(changes), len(items))
	return len(changes), nil
}

func (a *App) folderDescr(rel string) string {
	var d string
	a.cat.QueryRow("SELECT descr FROM tags WHERE facet = 'location' AND path = ?", rel).Scan(&d)
	return d
}

// classifyChanges turns the AI's answer into proposals, checked against the
// vocabulary: unknown kinds and machines that are neither there nor marked
// new under an existing parent are dropped.
func (a *App) classifyChanges(run string, items []classifyItem, ans classifyAnswer) []Change {
	known := map[int64]bool{}
	for _, it := range items {
		known[it.ID] = true
	}
	kinds := map[string]int64{}
	rows, err := a.cat.Query("SELECT id, name FROM tags WHERE facet = 'kind' AND merged_into IS NULL")
	if err == nil {
		for rows.Next() {
			var id int64
			var n string
			rows.Scan(&id, &n)
			kinds[strings.ToLower(n)] = id
		}
		rows.Close()
	}
	var out []Change
	for _, x := range ans.Items {
		if !known[x.ID] || x.Confidence < 0.5 {
			continue
		}
		target := targetOf("item", x.ID)
		reason := fmt.Sprintf("KI, Sicherheit %.0f %%", x.Confidence*100)
		if x.Note != "" {
			reason += ": " + x.Note
		}
		has := map[int64]bool{}
		weakKinds := []int64{}
		trows, err := a.cat.Query("SELECT it.tag, it.added_by, t.facet FROM item_tags it JOIN tags t ON t.id = it.tag WHERE it.item = ?", x.ID)
		if err == nil {
			for trows.Next() {
				var t int64
				var by, facet string
				trows.Scan(&t, &by, &facet)
				has[t] = true
				if facet == "kind" && by == "import:weak" {
					weakKinds = append(weakKinds, t)
				}
			}
			trows.Close()
		}
		add := func(facet, p string) {
			out = append(out, Change{Target: target, Op: "add-tag-path", Data: map[string]any{"facet": facet, "path": p, "by": run}, Rev: -1, Reason: reason})
		}
		if m := strings.TrimSpace(x.Machine); m != "" {
			id, err := a.resolveTag("Rechner: " + strings.ReplaceAll(m, " > ", " › "))
			switch {
			case err == nil && !has[id]:
				add("machine", strings.ReplaceAll(m, " › ", tagSep))
			case err != nil && x.NewMachine:
				names := splitTagPath(strings.ReplaceAll(m, " > ", " › "))
				if len(names) > 1 {
					if _, perr := a.resolveTag("Rechner: " + strings.Join(names[:len(names)-1], " › ")); perr == nil {
						add("machine", strings.Join(names, tagSep))
					}
				}
			}
		}
		if k := strings.ToLower(strings.TrimSpace(x.Kind)); k != "" {
			if id, ok := kinds[k]; ok && !has[id] {
				out = append(out, Change{Target: target, Op: "add-tag", Data: map[string]any{"tag": id, "by": run}, Rev: -1, Reason: reason})
				for _, w := range weakKinds {
					out = append(out, Change{Target: target, Op: "remove-tag", Data: map[string]any{"tag": w}, Rev: -1, Reason: reason})
				}
			}
		}
		if s := strings.TrimSpace(x.System); s != "" {
			add("system", s)
		}
		if l := strings.ToLower(strings.TrimSpace(x.Language)); langRe.MatchString(l) {
			add("language", l)
		}
	}
	return out
}

var langRe = regexp.MustCompile(`^[a-z]{2}$`)

var partRe = regexp.MustCompile(`(?i)^(.*?)[ _-]?(?:disk|disc|diskette|teil|part|vol|side)[ _-]?(\d{1,2}|[ab])(\.[a-z0-9]+)$`)

// proposeRelations proposes what needs no AI: items with the same bytes
// are the same, and the parts of a disk set or a split scan lying in one
// folder belong into one item.
func (a *App) proposeRelations(run, below string) (int, error) {
	lo, hi := pathRange(below)
	var changes []Change
	rows, err := a.cat.Query(`SELECT f.sha256, group_concat(DISTINCT f.item) FROM item_files f JOIN items i ON i.id = f.item
	    WHERE i.state = 'live' AND f.path >= ? AND f.path < ? GROUP BY f.sha256 HAVING COUNT(DISTINCT f.item) > 1`, lo, hi)
	if err != nil {
		return 0, err
	}
	var sets [][]int64
	for rows.Next() {
		var sha, ids string
		rows.Scan(&sha, &ids)
		var set []int64
		for _, s := range strings.Split(ids, ",") {
			n, _ := strconv.ParseInt(s, 10, 64)
			set = append(set, n)
		}
		sort.Slice(set, func(i, j int) bool { return set[i] < set[j] })
		sets = append(sets, set)
	}
	rows.Close()
	for _, set := range sets {
		for _, b := range set[1:] {
			var n int
			a.cat.QueryRow("SELECT COUNT(*) FROM item_rel WHERE a = ? AND kind = 'same-as' AND b = ?", b, set[0]).Scan(&n)
			if n == 0 {
				changes = append(changes, Change{Target: targetOf("item", b), Op: "add-rel", Data: map[string]any{"kind": "same-as", "item": set[0]}, Rev: -1, Reason: "gleiche Prüfsumme"})
			}
		}
	}

	// parts of a set: same stem and extension, numbered, in one folder
	rows, err = a.cat.Query(`SELECT f.item, f.path FROM item_files f JOIN items i ON i.id = f.item
	    WHERE i.state = 'live' AND i.root IS NULL AND f.path >= ? AND f.path < ?
	      AND (SELECT COUNT(*) FROM item_files g WHERE g.item = i.id) = 1 ORDER BY f.path`, lo, hi)
	if err != nil {
		return 0, err
	}
	groups := map[string][]int64{}
	var keys []string
	for rows.Next() {
		var id int64
		var p string
		rows.Scan(&id, &p)
		m := partRe.FindStringSubmatch(path.Base(p))
		if m == nil {
			continue
		}
		k := parentDir(p) + "\x00" + strings.ToLower(m[1]+m[3])
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], id)
	}
	rows.Close()
	for _, k := range keys {
		ids := groups[k]
		if len(ids) < 2 || len(ids) > 40 {
			continue
		}
		for _, id := range ids[1:] {
			changes = append(changes, Change{Target: targetOf("item", id), Op: "merge", Data: map[string]any{"into": ids[0]}, Rev: -1,
				Reason: "Teile eines Satzes im selben Ordner"})
		}
	}
	if len(changes) == 0 {
		return 0, nil
	}
	_, err = a.cat.Propose(run, changes)
	return len(changes), err
}

var (
	imageExt = regexp.MustCompile(`(?i)\.(jpe?g|png|gif|tiff?|bmp)$`)
	pageStem = regexp.MustCompile(`(?i)\d{1,4}[a-z]?$`)
	mediaExt = regexp.MustCompile(`(?i)\.(t64|tap|d64|d71|d81|g64|prg|crt|tzx|dsk|nib|po|do|2mg|adf|st|atr|xex|cas|wav|img|ima|imd|td0)$`)
)

// proposePages proposes to gather what a folder holds into one item, one
// batch per folder, where the folder is one thing: the pages of a
// document scanned to one image each (at least eight images, nearly all
// the folder holds, numbered like pages, of a document and not photos),
// or a cover tape or disk with the pictures of its inlay, which become its
// cover. In other folders, a picture named like a disk or tape image
// beside it becomes that image's cover, one batch each.
func (a *App) proposePages(run, below string) (int, error) {
	lo, hi := pathRange(below)
	rows, err := a.cat.Query(`SELECT f.item, f.path FROM item_files f JOIN items i ON i.id = f.item
	    WHERE i.state = 'live' AND i.root IS NULL AND f.path >= ? AND f.path < ?
	      AND (SELECT COUNT(*) FROM item_files g WHERE g.item = i.id) = 1
	      AND NOT EXISTS (SELECT 1 FROM revisions r WHERE r.target = 'item:' || i.id AND r.state = 'proposed')
	      AND i.id NOT IN (SELECT j.value FROM revisions r, json_each(r.data, '$.items') j WHERE r.op = 'gather' AND r.state = 'proposed')
	    ORDER BY f.path`, lo, hi)
	if err != nil {
		return 0, err
	}
	byDir := map[string][]page{}
	var dirs []string
	for rows.Next() {
		var id int64
		var p string
		rows.Scan(&id, &p)
		d, n := path.Split(p)
		d = strings.TrimSuffix(d, "/")
		if listingNames[strings.ToLower(n)] {
			continue
		}
		if _, ok := byDir[d]; !ok {
			dirs = append(dirs, d)
		}
		byDir[d] = append(byDir[d], page{id, n})
	}
	rows.Close()
	kindsOf := func(ids []int64) map[string]int {
		out := map[string]int{}
		q := "SELECT t.name FROM item_tags it JOIN tags t ON t.id = it.tag WHERE t.facet = 'kind' AND it.item IN (" + inList(len(ids)) + ")"
		r, err := a.cat.Query(q, int64Args(ids)...)
		if err != nil {
			return out
		}
		defer r.Close()
		for r.Next() {
			var k string
			r.Scan(&k)
			out[k]++
		}
		return out
	}
	n := 0
	for _, d := range dirs {
		ps := byDir[d]
		var imgs []page
		numbered := 0
		for _, p := range ps {
			if imageExt.MatchString(p.name) {
				imgs = append(imgs, p)
				if pageStem.MatchString(strings.TrimSuffix(p.name, path.Ext(p.name))) {
					numbered++
				}
			}
		}
		var all []int64
		media := 0
		for _, p := range ps {
			all = append(all, p.item)
			if mediaExt.MatchString(p.name) {
				media++
			}
		}
		kinds := kindsOf(all)
		natural := func(ps []page) {
			sort.SliceStable(ps, func(i, j int) bool { return naturalLess(strings.ToLower(ps[i].name), strings.ToLower(ps[j].name)) })
		}
		var ch Change
		switch {
		// the pages of a document, one image each
		case len(imgs) >= 8 && len(imgs)*10 >= len(ps)*9 && numbered*5 >= len(imgs)*4 && kinds["Fotos"] == 0 &&
			(kinds["Zeitschrift"] > 0 || kinds["Hersteller-Dokumentation"] > 0 || kinds["Sekundärliteratur"] > 0 || kinds["Buch"] > 0):
			natural(imgs)
			ch = gatherChange(imgs[0].item, imgs[1:], a.cat.folderLabel(d), false,
				fmt.Sprintf("%d Seiten eines Dokuments, im Ordner %s", len(imgs), d))
		// a cover tape or disk with the pictures of its inlay
		case media > 0 && len(imgs) > 0 && len(ps) <= 40 && kinds["Datenträger zum Heft"] > 0:
			natural(ps)
			sort.SliceStable(ps, func(i, j int) bool { return mediaExt.MatchString(ps[i].name) && !mediaExt.MatchString(ps[j].name) })
			ch = gatherChange(ps[0].item, ps[1:], a.cat.folderLabel(d), true,
				fmt.Sprintf("%d Abbilder mit %d Bildern des Einlegers, im Ordner %s", media, len(imgs), d))
		default:
			// a picture named like a disk or tape image beside it is its
			// label; the same disk in several formats is one item
			stemOf := func(p page) string { return strings.ToLower(strings.TrimSuffix(p.name, path.Ext(p.name))) }
			pics, disks := map[string][]page{}, map[string][]page{}
			var order []string
			for _, p := range ps {
				switch {
				case imageExt.MatchString(p.name):
					pics[stemOf(p)] = append(pics[stemOf(p)], p)
				case mediaExt.MatchString(p.name):
					if len(disks[stemOf(p)]) == 0 {
						order = append(order, stemOf(p))
					}
					disks[stemOf(p)] = append(disks[stemOf(p)], p)
				}
			}
			for _, stem := range order {
				if len(pics[stem]) == 0 {
					continue
				}
				ds := disks[stem]
				ch := gatherChange(ds[0].item, append(ds[1:], pics[stem]...), "", true,
					fmt.Sprintf("Bild des Etiketts von %s, im Ordner %s", ds[0].name, d))
				if _, err := a.cat.Propose(run, []Change{ch}); err != nil {
					return n, fmt.Errorf("%s: %w", d, err)
				}
				n++
			}
			continue
		}
		if _, err := a.cat.Propose(run, []Change{ch}); err != nil {
			return n, fmt.Errorf("%s: %w", d, err)
		}
		n++
	}
	return n, nil
}

// page is a file of a folder and the item it is.
type page struct {
	item int64
	name string
}

// gatherChange gathers the items into first, under the title.
func gatherChange(first int64, rest []page, title string, covers bool, reason string) Change {
	ids := make([]any, len(rest))
	for i, p := range rest {
		ids[i] = p.item
	}
	data := map[string]any{"items": ids, "title": title}
	if covers {
		data["covers"] = true
	}
	return Change{Target: targetOf("item", first), Op: "gather", Rev: -1, Data: data, Reason: reason}
}

// diskStem is a file's name without its extensions: "basic.imd.zip" is
// "basic".
var diskStem = regexp.MustCompile(`(?i)(\.[a-z0-9$]{1,4})+$`)

// proposeDisks proposes to gather what belongs to one disk into one item:
// its image, the same image in other formats, the log of the imaging, the
// photo of its label (its cover), and the folder of the same name the disk
// was unpacked into, whose files keep their path from there as name. One
// batch per disk.
func (a *App) proposeDisks(run, below string) (int, error) {
	lo, hi := pathRange(below)
	rows, err := a.cat.Query(`SELECT f.item, f.path FROM item_files f JOIN items i ON i.id = f.item
	    WHERE i.state = 'live' AND i.root IS NULL AND f.path >= ? AND f.path < ?
	      AND NOT EXISTS (SELECT 1 FROM revisions r WHERE r.target = 'item:' || i.id AND r.state = 'proposed')
	      AND i.id NOT IN (SELECT j.value FROM revisions r, json_each(r.data, '$.items') j WHERE r.op = 'gather' AND r.state = 'proposed')
	    ORDER BY f.path`, lo, hi)
	if err != nil {
		return 0, err
	}
	type file struct {
		item int64
		path string
	}
	var files []file
	byDir := map[string][]file{}   // the files directly in a folder
	below2 := map[string][]int64{} // the items of the files in a folder and below
	for rows.Next() {
		var f file
		rows.Scan(&f.item, &f.path)
		files = append(files, f)
		byDir[path.Dir(f.path)] = append(byDir[path.Dir(f.path)], f)
		for d := path.Dir(f.path); d != "." && d != ""; d = path.Dir(d) {
			below2[d] = append(below2[d], f.item)
		}
	}
	rows.Close()
	n := 0
	done := map[string]bool{}
	for _, f := range files {
		if !mediaExt.MatchString(f.path) {
			continue
		}
		dir := path.Dir(f.path)
		stem := diskStem.ReplaceAllString(path.Base(f.path), "")
		unpacked := dir + "/" + stem
		if done[unpacked] || len(below2[unpacked]) == 0 {
			continue
		}
		done[unpacked] = true
		seen := map[int64]bool{f.item: true}
		var rest []any
		take := func(id int64) {
			if !seen[id] {
				seen[id] = true
				rest = append(rest, id)
			}
		}
		for _, g := range byDir[dir] {
			if strings.EqualFold(diskStem.ReplaceAllString(path.Base(g.path), ""), stem) {
				take(g.item)
			}
		}
		for _, id := range below2[unpacked] {
			take(id)
		}
		ch := Change{Target: targetOf("item", f.item), Op: "gather", Rev: -1,
			Data:   map[string]any{"items": rest, "title": a.cat.folderLabel(unpacked), "covers": true, "base": dir},
			Reason: fmt.Sprintf("Diskette %s mit ihrem ausgepackten Inhalt (%d Dateien), im Ordner %s", stem, len(below2[unpacked]), dir)}
		if _, err := a.cat.Propose(run, []Change{ch}); err != nil {
			return n, fmt.Errorf("%s: %w", unpacked, err)
		}
		n++
	}
	return n, nil
}
