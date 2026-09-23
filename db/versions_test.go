package db

import "testing"

// What is stored is checked before it is written, because the digests are what references are
// raised on and the modes are what a runner lays the files out with.
func TestATreeIsCheckedBeforeItIsStored(t *testing.T) {
	for _, c := range []struct {
		name string
		file TreeFile
	}{
		{"a file with no path", TreeFile{SHA256: digestOf("e"), Mode: "0644"}},
		{"a digest that is not one", TreeFile{Path: "x", SHA256: "sha256:" + digestOf("e"), Mode: "0644"}},
		{"a mode git does not track", TreeFile{Path: "x", SHA256: digestOf("e"), Mode: "0600"}},
		{"a mode left out", TreeFile{Path: "x", SHA256: digestOf("e")}},
		{"a size below nothing", TreeFile{Path: "x", SHA256: digestOf("e"), Size: -1, Mode: "0644"}},
		{"a path named twice", aTree()[0]},
	} {
		if _, err := sortedTree(append(aTree(), c.file)); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
	if tree, err := sortedTree(nil); err != nil || tree != nil {
		t.Errorf("no tree answered %v, %v", tree, err)
	}
}
