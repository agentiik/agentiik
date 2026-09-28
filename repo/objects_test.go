package repo

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestObjectsEncodedHereHashAsGitHashesThem(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, nil, "init", "-q", ".")
	hashed := func(typ Type, data []byte) ID {
		t.Helper()
		id, err := ParseID(strings.TrimSpace(string(git(t, dir, data, "hash-object", "-t", typ.String(), "--stdin"))))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	blob := []byte("hello\n")
	if HashObject(TypeBlob, blob) != hashed(TypeBlob, blob) {
		t.Error("a blob hashes here as git does not hash it")
	}

	// Given in no order, with the names whose place is git's own: a directory a sorts as a/, so
	// after a-b and a.b, and before a0.
	entries := []TreeEntry{
		{Name: "a0", Mode: ModeFile, ID: HashObject(TypeBlob, []byte("a0"))},
		{Name: "a", Mode: ModeTree, ID: HashObject(TypeTree, nil)},
		{Name: "a.b", Mode: ModeExecutable, ID: HashObject(TypeBlob, []byte("a.b"))},
		{Name: "a-b", Mode: ModeSymlink, ID: HashObject(TypeBlob, []byte("a0"))},
		{Name: "sub", Mode: ModeSubmodule, ID: ID{0x12, 0x34}},
	}
	tree, err := EncodeTree(entries)
	if err != nil {
		t.Fatal(err)
	}
	var listing strings.Builder
	for _, e := range entries {
		typ := map[Mode]string{ModeTree: "tree", ModeSubmodule: "commit"}[e.Mode]
		if typ == "" {
			typ = "blob"
		}
		fmt.Fprintf(&listing, "%06s %s %s\t%s\n", e.Mode, typ, e.ID, e.Name)
	}
	mktree := strings.TrimSpace(string(git(t, dir, []byte(listing.String()), "mktree", "--missing")))
	if got := HashObject(TypeTree, tree).String(); got != mktree {
		t.Errorf("a tree hashes here as %s, and git mktree makes %s of the same entries", got, mktree)
	}
	if HashObject(TypeTree, tree) != hashed(TypeTree, tree) {
		t.Error("a tree hashes here as git hash-object does not hash it")
	}

	commit := &Commit{
		Tree:      HashObject(TypeTree, tree),
		Parents:   []ID{HashObject(TypeBlob, []byte("one")), HashObject(TypeBlob, []byte("two"))},
		Author:    sig,
		Committer: Signature{Name: "Grace Hopper", Email: "grace@example.com", When: 0, Zone: "-0000"},
		Headers:   []Header{{Key: "encoding", Value: "ISO-8859-1"}, {Key: "gpgsig", Value: "-----BEGIN-----\n\nabc\n-----END-----"}},
		Message:   "a merge\n\nwith a body\n",
	}
	data, err := commit.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if HashObject(TypeCommit, data) != hashed(TypeCommit, data) {
		t.Errorf("a commit hashes here as git does not hash it:\n%s", data)
	}

	tag := &Tag{Object: HashObject(TypeCommit, data), Type: TypeCommit, Name: "v1.0.0", Tagger: &sig, Message: "release\n"}
	if data, err = tag.Encode(); err != nil {
		t.Fatal(err)
	}
	if HashObject(TypeTag, data) != hashed(TypeTag, data) {
		t.Errorf("a tag hashes here as git does not hash it:\n%s", data)
	}
}

func TestEveryObjectGitWroteReadsBackAsItsOwnBytes(t *testing.T) {
	s := newSample(t)
	all := s.objects(t)
	contents := s.contents(t, slices.Collect(maps.Keys(all)))
	for id, typ := range all {
		data := contents[id]
		var again []byte
		var err error
		switch typ {
		case TypeCommit:
			var c *Commit
			if c, err = ParseCommit(data); err == nil {
				again, err = c.Encode()
			}
		case TypeTree:
			var entries []TreeEntry
			if entries, err = ParseTree(data); err == nil {
				again, err = EncodeTree(entries)
			}
		case TypeTag:
			var tag *Tag
			if tag, err = ParseTag(data); err == nil {
				again, err = tag.Encode()
			}
		default:
			continue
		}
		if err != nil {
			t.Errorf("the %s %s git wrote is refused: %s\n%s", typ, id, err, data)
			continue
		}
		if !bytes.Equal(again, data) {
			t.Errorf("the %s %s reads and writes back as other bytes:\n%q\n%q", typ, id, data, again)
		}
	}

	signed, err := ParseCommit(contents[gitID(t, s.dir, "signed")])
	if err != nil {
		t.Fatal(err)
	}
	if want := []Header{{Key: "gpgsig", Value: "-----BEGIN PGP SIGNATURE-----\n\niQEzBAABCAAdFiEE\n=abcd\n-----END PGP SIGNATURE-----"}}; !slices.Equal(signed.Headers, want) {
		t.Errorf("a signature over several lines reads as %q", signed.Headers)
	}
	if signed.Committer.Zone != "-0000" {
		t.Errorf("the zone -0000, which is not +0000, reads as %s", signed.Committer.Zone)
	}
	encoded, err := ParseCommit(contents[gitID(t, s.dir, "main")])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(encoded.Headers, []Header{{Key: "encoding", Value: "ISO-8859-1"}}) || encoded.Author != (Signature{"Ada Lovelace", "ada@example.com", 1700000000, "+0100"}) ||
		encoded.Committer != (Signature{"Grace Hopper", "grace@example.com", 1700000100, "+0000"}) || len(encoded.Parents) != 1 {
		t.Errorf("a commit git wrote reads as %+v", encoded)
	}
	tag, err := ParseTag(contents[gitID(t, s.dir, "tree-tag")])
	if err != nil {
		t.Fatal(err)
	}
	if tag.Type != TypeTree || tag.Name != "tree-tag" || tag.Tagger == nil || tag.Message != "a tree, tagged\n" {
		t.Errorf("a tag of a tree reads as %+v", tag)
	}
}

// refusal is an object this package refuses, the check it names, and whether git refuses it under
// the same name. git hash-object is asked, which runs git's fsck over what it is given and makes
// every finding an error, as fsck --strict does of all but badFilemode, which it only reports. The
// few git does not refuse are this package's own, the spellings git never writes among them.
type refusal struct {
	name  string
	typ   Type
	data  string
	check string
	git   bool
}

var (
	blobID  = HashObject(TypeBlob, []byte("x")).String()
	emptyID = HashObject(TypeTree, nil).String()
)

func entry(mode, name string, id ID) string { return mode + " " + name + "\x00" + string(id[:]) }

func refusedTrees() []refusal {
	blob := HashObject(TypeBlob, []byte("x"))
	empty := HashObject(TypeTree, nil)
	return []refusal{
		{"an entry with no space after its mode", TypeTree, "100644", checkBadTree, true},
		{"an entry whose name does not end", TypeTree, "100644 a", checkBadTree, true},
		{"an entry that ends inside its ID", TypeTree, "100644 a\x00" + string(blob[:10]), checkBadTree, true},
		{"a mode that is not octal", TypeTree, entry("100648", "a", blob), checkBadTree, true},
		{"a zero-padded mode", TypeTree, entry("040000", "a", empty), checkZeroPaddedFilemode, true},
		{"the mode 100664", TypeTree, entry("100664", "a", blob), checkBadFilemode, true},
		{"a mode that is none of git's", TypeTree, entry("100000", "a", blob), checkBadFilemode, true},
		{"an empty name", TypeTree, entry("100644", "", blob), checkBadTree, true},
		{"a name holding a slash", TypeTree, entry("100644", "a/b", blob), checkFullPathname, true},
		{"a name that is .", TypeTree, entry("40000", ".", empty), checkHasDot, true},
		{"a name that is ..", TypeTree, entry("40000", "..", empty), checkHasDotdot, true},
		{"a name that is .git", TypeTree, entry("40000", ".git", empty), checkHasDotgit, true},
		{"a name that is .GIT", TypeTree, entry("40000", ".GIT", empty), checkHasDotgit, true},
		{"a name that is .git on NTFS, dropping its trailing dots", TypeTree, entry("40000", ".git. .", empty), checkHasDotgit, true},
		{"a name that is .git on NTFS, as a stream", TypeTree, entry("100644", ".git::$INDEX_ALLOCATION", blob), checkHasDotgit, true},
		{"a name that is .git on NTFS, after a backslash", TypeTree, entry("40000", `a\.git`, empty), checkHasDotgit, true},
		{"the short name NTFS gives .git", TypeTree, entry("40000", "GIT~1", empty), checkHasDotgit, true},
		{"another short name NTFS may give .git", TypeTree, entry("40000", "git~12", empty), checkHasDotgit, false},
		{"a name that is .git on HFS+", TypeTree, entry("40000", ".g\u200cit", empty), checkHasDotgit, true},
		{"a name of 4097 bytes", TypeTree, entry("100644", strings.Repeat("a", 4097), blob), checkLargePathname, true},
		{"an entry naming no object", TypeTree, entry("100644", "a", ID{}), checkNullSha1, true},
		{".gitmodules as a symbolic link", TypeTree, entry("120000", ".gitmodules", blob), checkGitmodulesSymlink, true},
		{"the short name of .gitmodules as a symbolic link", TypeTree, entry("120000", "GITMOD~1", blob), checkGitmodulesSymlink, true},
		{".gitmodules as a directory", TypeTree, entry("40000", ".gitmodules", empty), checkGitmodulesBlob, false},
		{"a name twice", TypeTree, entry("100644", "a", blob) + entry("100644", "a", blob), checkDuplicateEntries, true},
		{"a file and a directory of one name, apart", TypeTree, entry("100644", "a", blob) + entry("100644", "a-b", blob) + entry("40000", "a", empty), checkDuplicateEntries, true},
		{"names out of order", TypeTree, entry("100644", "b", blob) + entry("100644", "a", blob), checkTreeNotSorted, true},
		{"a directory before a name it sorts after", TypeTree, entry("40000", "a", empty) + entry("100644", "a.b", blob), checkTreeNotSorted, true},
	}
}

func head(author string) string {
	return "tree " + emptyID + "\nauthor " + author + "\ncommitter C <c@example.com> 1 +0000\n\nmessage\n"
}

func refusedCommits() []refusal {
	return []refusal{
		{"a null byte among the headers", TypeCommit, "tree " + emptyID + "\x00\n", checkNulInHeader, true},
		{"headers that do not end", TypeCommit, "tree " + emptyID, checkUnterminatedHeader, true},
		{"no tree line", TypeCommit, "parent " + blobID + "\n" + head("A <a@example.com> 1 +0000")[len("tree "+emptyID+"\n"):], checkMissingTree, true},
		{"a tree line naming no object", TypeCommit, "tree 1234\nauthor A <a@example.com> 1 +0000\ncommitter C <c@example.com> 1 +0000\n\n", checkBadTreeSha1, true},
		{"a tree ID in upper case", TypeCommit, "tree " + strings.ToUpper(emptyID) + "\nauthor A <a@example.com> 1 +0000\ncommitter C <c@example.com> 1 +0000\n\n", checkBadTreeSha1, false},
		{"a parent line naming no object", TypeCommit, "tree " + emptyID + "\nparent xyz\nauthor A <a@example.com> 1 +0000\ncommitter C <c@example.com> 1 +0000\n\n", checkBadParentSha1, true},
		{"no author", TypeCommit, "tree " + emptyID + "\ncommitter C <c@example.com> 1 +0000\n\n", checkMissingAuthor, true},
		{"two authors", TypeCommit, "tree " + emptyID + "\nauthor A <a@example.com> 1 +0000\nauthor A <a@example.com> 1 +0000\ncommitter C <c@example.com> 1 +0000\n\n", checkMultipleAuthors, true},
		{"no committer", TypeCommit, "tree " + emptyID + "\nauthor A <a@example.com> 1 +0000\n\n", checkMissingCommitter, true},
		{"a null byte in the message", TypeCommit, head("A <a@example.com> 1 +0000") + "\x00", checkNulInCommit, true},
		{"no name before the email", TypeCommit, head("<a@example.com> 1 +0000"), checkMissingNameBeforeEmail, true},
		{"no email", TypeCommit, head("A 1 +0000"), checkMissingEmail, true},
		{"a > in the name", TypeCommit, head("A> <a@example.com> 1 +0000"), checkBadName, true},
		{"no space before the email", TypeCommit, head("A<a@example.com> 1 +0000"), checkMissingSpaceBeforeEmail, true},
		{"an email that does not end", TypeCommit, head("A <a@example.com 1 +0000"), checkBadEmail, true},
		{"a < in the email", TypeCommit, head("A <a<b@example.com> 1 +0000"), checkBadEmail, true},
		{"no space before the date", TypeCommit, head("A <a@example.com>1 +0000"), checkMissingSpaceBeforeDate, true},
		{"a zero-padded date", TypeCommit, head("A <a@example.com> 01 +0000"), checkZeroPaddedDate, true},
		{"no date", TypeCommit, head("A <a@example.com> +0000"), checkBadDate, true},
		{"a date with a sign", TypeCommit, head("A <a@example.com> +1 +0000"), checkBadDate, true},
		{"a date after two spaces", TypeCommit, head("A <a@example.com>  1 +0000"), checkBadDate, false},
		{"a date past 64 bits", TypeCommit, head("A <a@example.com> 99999999999999999999 +0000"), checkBadDateOverflow, true},
		{"a zone of three digits", TypeCommit, head("A <a@example.com> 1 +000"), checkBadTimezone, true},
		{"a zone with no sign", TypeCommit, head("A <a@example.com> 1 0000"), checkBadTimezone, true},
		{"a zone of five digits", TypeCommit, head("A <a@example.com> 1 +00000"), checkBadTimezone, true},
	}
}

func refusedTags() []refusal {
	rest := "type blob\ntag v1\ntagger T <t@example.com> 1 +0000\n\nmessage\n"
	return []refusal{
		{"a null byte among the headers", TypeTag, "object " + blobID + "\x00\n" + rest, checkNulInHeader, true},
		{"headers that do not end", TypeTag, "object " + blobID, checkUnterminatedHeader, true},
		{"no object line", TypeTag, rest, checkMissingObject, true},
		{"an object line naming no object", TypeTag, "object 12\n" + rest, checkBadObjectSha1, true},
		{"no type line", TypeTag, "object " + blobID + "\ntag v1\n\n", checkMissingTypeEntry, true},
		{"a type that is none of git's", TypeTag, "object " + blobID + "\ntype thing\ntag v1\n\n", checkBadType, true},
		{"no tag line", TypeTag, "object " + blobID + "\ntype blob\ntagger T <t@example.com> 1 +0000\n\n", checkMissingTagEntry, true},
		{"a tagger with no email", TypeTag, "object " + blobID + "\ntype blob\ntag v1\ntagger T 1 +0000\n\n", checkMissingEmail, true},
	}
}

func TestObjectsGitFsckRefusesAreRefusedUnderItsName(t *testing.T) {
	dir := t.TempDir()
	for _, c := range slices.Concat(refusedTrees(), refusedCommits(), refusedTags()) {
		t.Run(c.typ.String()+" with "+c.name, func(t *testing.T) {
			var err error
			switch c.typ {
			case TypeTree:
				_, err = ParseTree([]byte(c.data))
			case TypeCommit:
				_, err = ParseCommit([]byte(c.data))
			case TypeTag:
				_, err = ParseTag([]byte(c.data))
			}
			var oe *ObjectError
			if !errors.As(err, &oe) || oe.Check != c.check || oe.Type != c.typ {
				t.Fatalf("read with %v, where %s is refused as %s", err, c.name, c.check)
			}
			if !strings.HasSuffix(err.Error(), "("+c.check+")") {
				t.Errorf("the refusal reads %q, and does not name its check", err)
			}
			if !c.git {
				return
			}
			// git hash-object runs git's fsck over what it is given, as receive-pack does over what
			// it is pushed with receive.fsckObjects, and names the check that failed.
			_, err = gitErr(dir, []byte(c.data), nil, "hash-object", "-t", c.typ.String(), "--stdin")
			if err == nil || !strings.Contains(err.Error(), "object fails fsck: "+c.check+":") {
				t.Errorf("git hash-object says %v, and not %s", err, c.check)
			}
		})
	}
}

func TestObjectsGitFsckLetsByAreRead(t *testing.T) {
	blob := HashObject(TypeBlob, []byte("x"))
	for name, c := range map[string]struct {
		typ  Type
		data string
	}{
		"a commit with no message and no blank line": {TypeCommit, "tree " + emptyID + "\nauthor A <a@example.com> 1 +0000\ncommitter C <c@example.com> 1 +0000\n"},
		"a commit by nobody named":                   {TypeCommit, "tree " + emptyID + "\nauthor  <a@example.com> 0 +0000\ncommitter C <> 1 -0000\n\n"},
		"a commit with a header of no value":         {TypeCommit, "tree " + emptyID + "\nauthor A <a@example.com> 1 +0000\ncommitter C <c@example.com> 1 +0000\nmergetag\n\n"},
		"a tag with no tagger, as the first ones":    {TypeTag, "object " + blobID + "\ntype blob\ntag v2.6.11\n\nmessage\n"},
		"a tag with headers after its tagger":        {TypeTag, "object " + blobID + "\ntype blob\ntag v1\ntagger T <t@example.com> 1 +0000\nextra header\n\n"},
		"a tag named what no ref may be":             {TypeTag, "object " + blobID + "\ntype blob\ntag a..b\n\n"},
		"an empty tree":                              {TypeTree, ""},
		"a name that is .git further in":             {TypeTree, entry("100644", ".gitx", blob) + entry("100644", "a.git", blob)},
		".gitmodules as a file":                      {TypeTree, entry("100755", ".gitmodules", blob)},
		"a symbolic link named like .gitattributes":  {TypeTree, entry("120000", ".gitattributes", blob)},
	} {
		var err error
		switch c.typ {
		case TypeTree:
			_, err = ParseTree([]byte(c.data))
		case TypeCommit:
			_, err = ParseCommit([]byte(c.data))
		case TypeTag:
			_, err = ParseTag([]byte(c.data))
		}
		if err != nil {
			t.Errorf("%s is refused: %s", name, err)
		}
	}
}

func TestAHeaderOfManyLinesIsReadInOnePass(t *testing.T) {
	// Joined a line at a time, a header of 20,000 lines would copy what it had read at each line,
	// some 600 MB for a commit of 60 KB; joined once, it costs a few times its weight.
	const n = 20000
	var b strings.Builder
	b.WriteString("tree " + emptyID + "\nauthor A <a@example.com> 1 +0000\ncommitter C <c@example.com> 1 +0000\ngpgsig x\n")
	for range n {
		b.WriteString(" y\n")
	}
	b.WriteString("\nsigned\n")
	data := []byte(b.String())
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	c, err := ParseCommit(data)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if want := "x" + strings.Repeat("\ny", n); len(c.Headers) != 1 || c.Headers[0].Value != want {
		t.Fatalf("the header reads as %d headers", len(c.Headers))
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 100*uint64(len(data)) {
		t.Errorf("reading a commit of %d bytes allocated %d", len(data), allocated)
	}
}

func TestATreeIsCheckedWithoutBeingHeldTwice(t *testing.T) {
	// A hundred thousand entries of four-letter names: kept as entries and a map of names, they
	// would take several times the tree's weight; checked, they are let go one by one.
	var entries []TreeEntry
	for i := range 100000 {
		entries = append(entries, TreeEntry{Name: fmt.Sprintf("%04x", i), Mode: ModeFile, ID: HashObject(TypeBlob, nil)})
	}
	entries = append(entries, TreeEntry{Name: "a", Mode: ModeFile, ID: HashObject(TypeBlob, nil)}, TreeEntry{Name: "a-b", Mode: ModeFile, ID: HashObject(TypeBlob, nil)})
	tree, err := EncodeTree(entries)
	if err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err = checkTree(tree)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > uint64(len(tree)) {
		t.Errorf("checking a tree of %d bytes allocated %d", len(tree), allocated)
	}
	// And still finds a name twice where the two are apart, which git's order puts them.
	entries = append(entries, TreeEntry{Name: "a", Mode: ModeTree, ID: HashObject(TypeTree, nil)})
	slices.SortStableFunc(entries, compareEntries)
	var twice []byte
	for _, e := range entries {
		twice = append(twice, entry(e.Mode.String(), e.Name, e.ID)...)
	}
	var oe *ObjectError
	if err := checkTree(twice); !errors.As(err, &oe) || oe.Check != checkDuplicateEntries {
		t.Errorf("a file a, a file a-b and a directory a check with %v", err)
	}
}

func TestEncodingRefusesWhatWouldNotReadBackAsItself(t *testing.T) {
	blob := HashObject(TypeBlob, []byte("x"))
	for name, err := range map[string]error{
		"a name holding a null byte": func() error {
			_, err := EncodeTree([]TreeEntry{{Name: "a\x00b", Mode: ModeFile, ID: blob}})
			return err
		}(),
		"a name twice": func() error {
			_, err := EncodeTree([]TreeEntry{{Name: "a", Mode: ModeFile, ID: blob}, {Name: "a", Mode: ModeFile, ID: blob}})
			return err
		}(),
		"a mode that is none of git's": func() error {
			_, err := EncodeTree([]TreeEntry{{Name: "a", Mode: 0o100664, ID: blob}})
			return err
		}(),
		"an author holding a line feed": func() error {
			_, err := (&Commit{Author: Signature{Name: "A\nB", Zone: "+0000"}, Committer: sig}).Encode()
			return err
		}(),
		"a time before the epoch": func() error {
			_, err := (&Commit{Author: Signature{Name: "A", When: -1, Zone: "+0000"}, Committer: sig}).Encode()
			return err
		}(),
		"a zone of no sign": func() error {
			_, err := (&Commit{Author: Signature{Name: "A", Zone: "0100"}, Committer: sig}).Encode()
			return err
		}(),
		"a header key holding a space": func() error {
			_, err := (&Commit{Author: sig, Committer: sig, Headers: []Header{{Key: "a b", Value: "c"}}}).Encode()
			return err
		}(),
		"a second header with no key": func() error {
			_, err := (&Commit{Author: sig, Committer: sig, Headers: []Header{{Key: "a", Value: "b"}, {Value: "c"}}}).Encode()
			return err
		}(),
		"a tag name holding a line feed": func() error {
			_, err := (&Tag{Type: TypeBlob, Name: "a\nb"}).Encode()
			return err
		}(),
		"a header named tagger in a tag with none": func() error {
			_, err := (&Tag{Type: TypeBlob, Name: "v", Headers: []Header{{Key: "tagger", Value: sig.String()}}}).Encode()
			return err
		}(),
	} {
		if err == nil {
			t.Errorf("%s is written", name)
		}
	}
}

func TestDotGitIsGitsRuleOnEveryFilesystem(t *testing.T) {
	for name, want := range map[string]bool{
		".git": true, ".GIT": true, ".Git": true, ".git.": true, ".git ": true, ".git . .": true,
		".git::$INDEX_ALLOCATION": true, ".git:": true, "GIT~1": true, "git~1": true, "GIT~2": true, "git~10": true,
		".g\u200cit": true, "\ufeff.git": true, ".gi\u206ft": true, `a\.git`: true, `.git\a`: true, `a\GIT~1\b`: true,
		".gitx": false, "x.git": false, "git": false, ".gi": false, "GIT~": false, "GIT~1a": false, "git~x": false,
		"..git": false, ".g it": false, ".gitmodules": false, `a\b`: false, ".g\u0130t": false,
	} {
		if got := DotGit(name); got != want {
			t.Errorf("DotGit(%q) = %t", name, got)
		}
	}
	for name, want := range map[string]bool{
		".gitmodules": true, ".GITMODULES": true, ".gitmodules.": true, ".gitmodules:x": true, ".git\u200bmodules": false,
		".gitmod\u200cules": true, "GITMOD~1": true, "gitmod~4": true, "gitmod~5": false, "GI7EBA~1": true, "gi7eba~9": true,
		"gi7eb~12": true, "g~123456": true, "gi7eba~0": false, "gi7ebb~1": false, "gitmodules": false, `a\.gitmodules`: true,
		".gitmodule\u017f": false, ".gitmodu\u212ales": false,
	} {
		if got := dotGitmodules(name); got != want {
			t.Errorf("dotGitmodules(%q) = %t", name, got)
		}
	}
}

func TestAnIDIsFortyLowerCaseHexadecimalDigits(t *testing.T) {
	id := HashObject(TypeBlob, nil)
	if got, err := ParseID(id.String()); err != nil || got != id {
		t.Errorf("ParseID(%s) = %s, %v", id, got, err)
	}
	for _, s := range []string{"", strings.ToUpper(id.String()), id.String()[:39], id.String() + "0", strings.Repeat("g", 40)} {
		if _, err := ParseID(s); err == nil {
			t.Errorf("ParseID(%q) reads it", s)
		}
	}
}
