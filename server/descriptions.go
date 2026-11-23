package archiv

import (
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// Filepage left a "descriptions" file in most folders: one line per entry,
// "<name><blank><description>", an optional "title=" line naming the folder
// itself, CRLF line ends, mostly cp1252.

// decodeCP1252 reads UTF-8 where the bytes are valid UTF-8, cp1252 otherwise.
func decodeCP1252(raw []byte) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	s, err := charmap.Windows1252.NewDecoder().Bytes(raw)
	if err != nil {
		return strings.ToValidUTF8(string(raw), "�")
	}
	return string(s)
}

// parseDescriptions returns the folder title and the entry lines of a
// descriptions file, verbatim; matchEntries maps them onto real names.
func parseDescriptions(raw []byte) (title string, lines []string) {
	seenTitle := false
	for _, line := range splitLines(decodeCP1252(raw)) {
		line = strings.TrimRightFunc(line, isSpace)
		if line == "" {
			continue
		}
		if !seenTitle && strings.HasPrefix(strings.ToLower(line), "title=") {
			title = strings.TrimSpace(line[len("title="):])
			seenTitle = true
			continue
		}
		lines = append(lines, line)
	}
	return title, lines
}

// readDescriptions parses the descriptions file of dir; a missing file is
// an empty one.
func readDescriptions(dir string) (string, []string) {
	raw, err := os.ReadFile(dir + "/descriptions")
	if err != nil {
		return "", nil
	}
	return parseDescriptions(raw)
}

// matchEntries maps description lines onto the entries of a folder. Names
// may contain blanks, and the lines keep the original mixed case while the
// names on disk are mostly lower case, so a line belongs to the longest name
// it starts with, case-insensitively, followed by a blank or the line end.
// The first non-empty description of a name wins; one of only quotes (")
// repeats the description of the line above, as Filepage lists used it.
func matchEntries(lines, names []string) map[string]string {
	// one name per case-folded spelling, the first one given
	var ordered []string
	seen := map[string]bool{}
	for _, n := range names {
		if l := strings.ToLower(n); !seen[l] {
			seen[l] = true
			ordered = append(ordered, n)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })

	out := map[string]string{}
	prev := "" // a description of only quotes (") means: as the line above
	for _, line := range lines {
		for _, name := range ordered {
			if len(line) < len(name) || !strings.EqualFold(line[:len(name)], name) {
				continue
			}
			rest := line[len(name):]
			if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
				continue
			}
			if d := strings.TrimSpace(rest); d != "" {
				if strings.Trim(d, `"'„“” -`) == "" {
					d = prev
				} else {
					d = dittoWords(d, prev)
				}
				prev = d
				if _, ok := out[name]; !ok && d != "" {
					out[name] = d
				}
			}
			break
		}
	}
	return out
}

// splitLines splits at \n, \r\n and lone \r, like Python's splitlines for
// the line ends that occur in these files.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\f' || r == '\v' }

// dittoWords fills in words given as a lone quote (") from the same
// position of the line above: `" " " Part 3/3` after
// `Small C compiler, Part 2/3`.
func dittoWords(d, prev string) string {
	words := strings.Fields(d)
	pw := strings.Fields(prev)
	changed := false
	for i, w := range words {
		if w == `"` && i < len(pw) {
			words[i] = pw[i]
			changed = true
		}
	}
	if !changed {
		return d
	}
	return strings.Join(words, " ")
}
