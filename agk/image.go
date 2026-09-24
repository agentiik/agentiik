package agk

import (
	"regexp"
	"strings"
)

// An image reference, and when it names what runs.
//
// "Images by digest in production: a tag is a mutable pointer, and a commit must determine
// what ran." So a server run names every image the way wire.schema.json's imageRef does, a
// repository and the sha256 digest of the manifest its registry serves under it, and agk push
// resolves a tag to one before a version is recorded. A local run keeps the tag: an image
// built on the machine and never pushed has no registry to be named by.

// imageByDigest is imageRef's pattern, written as the wire writes it, and a test holds the two
// to the same answers.
var imageByDigest = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

// ImageByDigest says whether ref names its image by digest, as a task message has to. A tag
// written beside the digest, alpine:3.21@sha256:..., is accepted as the wire accepts it, since
// the digest is what is pulled and the tag says nothing a digest does not.
func ImageByDigest(ref string) bool { return imageByDigest.MatchString(ref) }

// ImageRepository is the repository ref names, written as ref writes it: ref less its digest
// and its tag.
//
// Only a colon after the last slash begins a tag, because registry.example:5000/brick names a
// port and no tag at all.
func ImageRepository(ref string) string {
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		ref = ref[:at]
	}
	if colon := strings.LastIndexByte(ref, ':'); colon > strings.LastIndexByte(ref, '/') {
		ref = ref[:colon]
	}
	return ref
}
