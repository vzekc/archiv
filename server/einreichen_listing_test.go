package archiv

import "testing"

func TestParse7zListing(t *testing.T) {
	bz2 := "Size = \nPacked Size = \n\n"
	iso := "Path = sub\nFolder = +\nSize = \nPacked Size = \n\n" +
		"Path = sub/a.txt\nFolder = -\nSize = 3\nPacked Size = 3\n\n"
	zip := "Path = sub\nFolder = +\nSize = 0\nAttributes = D_ drwxr-xr-x\n\n" +
		"Path = sub/a.txt\nFolder = -\nSize = 3\nAttributes = _ -rw-r--r--\n"

	names, total, known := parse7zListing(bz2)
	if len(names) != 0 || known {
		t.Errorf("bz2: names %v known %v, want none and unknown", names, known)
	}
	if unpackLimit(FileInfo{Unpacked: 0}) < 1<<30 {
		t.Errorf("unknown size must not cap unpacking at %d", unpackLimit(FileInfo{}))
	}
	for name, out := range map[string]string{"iso": iso, "zip": zip} {
		names, total, known = parse7zListing(out)
		if len(names) != 1 || names[0] != "sub/a.txt" || total != 3 || !known {
			t.Errorf("%s: got %v %d %v", name, names, total, known)
		}
	}
	if got := unpackLimit(FileInfo{Unpacked: 100 << 20}); got != 200<<20 {
		t.Errorf("limit for 100 MB: %d", got)
	}
}
