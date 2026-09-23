package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/artifact"
)

// The tree a push carries, which is what every step of every run sees.

func pushed(t *testing.T, tree map[string]api.PushFile) api.Push {
	t.Helper()
	p := aPush(t)
	p.Tree = tree
	return p
}

func TestAPushStoresItsTreeAsObjectsAndNamesThem(t *testing.T) {
	h, _, _, objects := servingWithObjects(t)

	body := pushed(t, map[string]api.PushFile{
		"agentiik.yaml":      {Content: []byte("the entry point")},
		"scripts/render.sh":  {Content: []byte("#!/bin/sh\necho hello\n"), Mode: "0755"},
		"fragments/base.yml": {Content: []byte("a fragment")},
	})
	w, _ := call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/a3f9c1e", "alice", body)
	if w.Code != http.StatusOK {
		t.Fatalf("pushing answered %d: %s", w.Code, w.Body)
	}

	// Every file is an object in the namespace, under the digest of its own bytes.
	for path, f := range body.Tree {
		sum := sha256.Sum256(f.Content)
		key := artifact.Key("finance", hex.EncodeToString(sum[:]))
		held, err := objects.Has(context.Background(), key)
		if err != nil || !held {
			t.Errorf("%s is not in the store: %v %v", path, held, err)
		}
	}

	// A file two commits share is one object, so a version costs what changed.
	second := pushed(t, map[string]api.PushFile{
		"agentiik.yaml":     {Content: []byte("the entry point")},
		"scripts/render.sh": {Content: []byte("#!/bin/sh\necho goodbye\n"), Mode: "0755"},
	})
	w, _ = call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/b4a0d2f", "alice", second)
	if w.Code != http.StatusOK {
		t.Fatalf("the second push answered %d: %s", w.Code, w.Body)
	}
	sum := sha256.Sum256([]byte("the entry point"))
	if held, err := objects.Has(context.Background(), artifact.Key("finance", hex.EncodeToString(sum[:]))); err != nil || !held {
		t.Errorf("the shared file is not in the store: %v %v", held, err)
	}
}

func TestATreeThatCannotBeGivenToAContainer(t *testing.T) {
	h, _, _, _ := servingWithObjects(t)

	for _, c := range []struct {
		name string
		tree map[string]api.PushFile
	}{
		{"a path that leaves the repository", map[string]api.PushFile{
			"../outside.sh": {Content: []byte("x")},
		}},
		{"an absolute path", map[string]api.PushFile{
			"/etc/passwd": {Content: []byte("x")},
		}},
		{"a path that is not in its cleaned form", map[string]api.PushFile{
			"scripts/../scripts/render.sh": {Content: []byte("x")},
		}},
		{"a mode a tree does not carry", map[string]api.PushFile{
			"render.sh": {Content: []byte("x"), Mode: "4755"},
		}},
		{"a repository above the limit", map[string]api.PushFile{
			"big.bin": {Content: make([]byte, api.TreeMaxBytes+1)},
		}},
	} {
		w, answer := call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/a3f9c1e",
			"alice", pushed(t, c.tree))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d", c.name, w.Code)
			continue
		}
		if said, _ := answer["error"].(string); said == "" {
			t.Errorf("%s was refused with no reason", c.name)
		}
	}
}

// A refusal about size says what the limit is for, because a limit with no rationale is a limit
// somebody works around rather than reconsiders.
func TestTheSizeRefusalSaysWhyThereIsALimit(t *testing.T) {
	h, _, _, _ := servingWithObjects(t)
	w, answer := call(t, h, "PUT", "/api/v1/finance/workflows/monthly-invoicing/versions/a3f9c1e",
		"alice", pushed(t, map[string]api.PushFile{"big.bin": {Content: make([]byte, api.TreeMaxBytes+1)}}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an oversized tree answered %d", w.Code)
	}
	said, _ := answer["error"].(string)
	if !strings.Contains(said, "belongs in an image or in an artifact") {
		t.Errorf("the refusal reads %q", said)
	}
}
