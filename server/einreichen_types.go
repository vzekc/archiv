package archiv

import (
	"encoding/json"
	"slices"
	"strings"
)

// FileInfo is what processing found out about a file (files.info).
type FileInfo struct {
	Pages        int      `json:"pages,omitempty"`
	Text         string   `json:"text,omitempty"`    // excerpt of the content
	Listing      []string `json:"listing,omitempty"` // names inside an archive
	ListingTotal int      `json:"listing_total,omitempty"`
	Unpacked     int64    `json:"unpacked,omitempty"` // total size of the contents
	SingleRoot   string   `json:"single_root,omitempty"`
	Format       string   `json:"format,omitempty"` // what `file` says
	Thumb        bool     `json:"thumb,omitempty"`
	Preview      bool     `json:"preview,omitempty"`
	Virus        string   `json:"virus,omitempty"`
}

// Duplicate is a file with the same content found elsewhere.
type Duplicate struct {
	Where      string `json:"where"` // archive, pending
	Path       string `json:"path"`
	Submission int64  `json:"submission,omitempty"`
}

// Suggestion is the AI's answer for one file (files.suggestion). The tags,
// reason and notes come from the file's group and are the same for every
// file in it; Description, Language and FileNote are the file's own.
type Suggestion struct {
	File              string   `json:"file"`
	Group             string   `json:"group,omitempty"`    // label of the group
	Tags              []string `json:"tags"`               // "Facette: A › B"
	NewTags           []string `json:"new_tags,omitempty"` // those of Tags that do not exist yet
	Confidence        float64  `json:"confidence"`
	Description       string   `json:"description"`
	Language          string   `json:"language"`
	Reason            string   `json:"reason"`
	NotesForModerator string   `json:"notes_for_moderator"` // about the group
	FileNote          string   `json:"file_note,omitempty"` // about this file only
	Unpack            bool     `json:"unpack"`
	Final             bool     `json:"final,omitempty"`
	Model             string   `json:"model,omitempty"`
	Time              int64    `json:"time,omitempty"`
}

// GroupKey identifies the files that share one suggestion.
func (g *Suggestion) GroupKey() string {
	if g == nil {
		return ""
	}
	return g.Group + "\x00" + strings.Join(g.Tags, "\n")
}

// IsNew tells whether a suggested tag does not exist yet.
func (g *Suggestion) IsNew(tag string) bool {
	return slices.Contains(g.NewTags, tag)
}

// tagLines splits the tags of a file, one per line.
func tagLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func parseInfo(s string) FileInfo {
	var i FileInfo
	if s != "" {
		json.Unmarshal([]byte(s), &i)
	}
	return i
}

func parseDuplicates(s string) []Duplicate {
	var d []Duplicate
	if s != "" {
		json.Unmarshal([]byte(s), &d)
	}
	return d
}

func parseSuggestion(s string) *Suggestion {
	if s == "" {
		return nil
	}
	var g Suggestion
	if json.Unmarshal([]byte(s), &g) != nil {
		return nil
	}
	return &g
}
