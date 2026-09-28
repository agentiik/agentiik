package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/api"
	"github.com/agentiik/agentiik/db"
	"github.com/agentiik/agentiik/graph"
	versions "github.com/agentiik/agentiik/version"
)

// What agk read of the images a workflow names, recorded on the installation.
//
// "A git push reaches no registry": the pre-receive hook judges a pushed commit against the digest
// its namespace pinned each tag to and the manifest it recorded for each image, and refuses a tag or
// an image it holds nothing for, naming agk push. So agk push, and agk validate where an
// installation is configured, send what the daemon on this machine read with PUT
// /api/v1/{ns}/images, the one place the installation learns it, since it reaches no registry and
// pulls no image.

// recordable is what a check of a commit read of its images, as the installation records it: each
// tag at the digest it was pinned to, and each manifest by the image it was read out of, named by
// digest, which is the digest its tag was pinned to where a step wrote a tag.
func recordable(v db.Version) api.Images {
	out := api.Images{Pins: maps.Clone(v.Images), Manifests: map[string][]byte{}}
	for written, document := range v.Manifests {
		image := written
		if pinned, held := v.Images[written]; held {
			image = pinned
		}
		if agk.ImageByDigest(image) {
			out.Manifests[image] = document
		}
	}
	return out
}

// recordImages sends a record of images to the namespace, and answers why it was not kept where it
// was not, in the words a person acts on.
func recordImages(ctx context.Context, at remote, namespace string, images api.Images) error {
	err := at.sendJSON(ctx, http.MethodPut, "/api/v1/"+namespace+"/images", images, http.StatusNoContent, nil)
	switch statusOf(err) {
	case 0:
		return err
	case http.StatusUnauthorized:
		return errors.New(at.refusedCredential())
	case http.StatusNotFound:
		// The answer a namespace nobody created gets, which is the point: nothing tells the two
		// apart, and saying which permission it takes is saying what can be done about it.
		return fmt.Errorf("the installation holds no namespace %s you hold workflow:write on as a whole, which recording them takes", namespace)
	}
	return err
}

// recorded is the line saying what the installation now holds of a workflow's images, where being
// the namespace, and the installation where the line does not say it otherwise.
func recorded(images api.Images, where string) string {
	return fmt.Sprintf("%s and %s recorded in %s, which its git pushes are judged against",
		counted(len(images.Pins), "tag pinned", "tags pinned"), counted(len(images.Manifests), "brick manifest", "brick manifests"), where)
}

// installationConfigured says whether an installation is named for agk to talk to, by
// AGENTIIK_SERVER or by agk login having signed in to one, and says nothing where none is: agk
// validate reads a working tree and needs no installation, and records what it read only where one
// is. A profile that cannot be read is one that may name one, and reach says why it cannot.
func installationConfigured(e Env) bool {
	if e.getenv(serverVariable) != "" {
		return true
	}
	last, err := lastSignedIn(e)
	return err != nil || last != ""
}

// recordValidated records on the installation agk is configured for what agk validate read of the
// images a workflow's bricks run, where one is configured and the workflow is valid: each tag at the
// digest its registry serves it under, and the manifest read out of that digest, so that a git push
// of what was validated here is judged against what was read here.
//
// The bricks' images alone, since those are what validate has the daemon read: the base image of a
// script step is pulled by nothing here, and agk push pins it. A tag the registry cannot confirm, an
// image built on this machine and never pushed among them, is recorded nowhere, which a line says:
// the file is no less valid for it, and the push is what refuses it. Nothing here
// changes how validate exits, since whether a file is valid is no question of whether an
// installation could be reached.
func recordValidated(ctx context.Context, e Env, images *daemon, checked *versions.Checked, namespace string) {
	wf := checked.Workflow
	bricks := graph.Images(wf)
	if len(bricks) == 0 || !installationConfigured(e) {
		return
	}
	if namespace == "" {
		namespace = wf.Metadata.Namespace
	}
	if namespace == "" {
		fmt.Fprintln(e.Out, "What was read of the images was recorded nowhere: the workflow names no namespace, and --namespace names the one to record it in")
		return
	}
	// Asked before anything else is, so that a credential that is not there costs no registry a
	// question; and quietly, since what reach says is why nothing was recorded.
	var why strings.Builder
	quiet := e
	quiet.Err = &why
	at, ok := reach(quiet, "")
	if !ok {
		fmt.Fprintf(e.Err, "what was read of the images was not recorded in %s: %s\n", namespace, strings.TrimSpace(why.String()))
		return
	}

	first := map[string]agk.Step{}
	for _, name := range slices.Sorted(maps.Keys(wf.Steps)) {
		if st := wf.Steps[name]; st.Image != "" && len(st.Script) == 0 {
			if _, held := first[st.Image]; !held {
				first[st.Image] = name
			}
		}
	}
	record := api.Images{Pins: map[string]string{}, Manifests: map[string][]byte{}}
	for _, image := range bricks {
		if agk.ImageByDigest(image) {
			record.Manifests[image] = checked.Version.Manifests[image]
			continue
		}
		// Read again out of the digest the tag was pinned to rather than kept as it was read
		// by the tag, so that a tag moved on this machine in between cannot pair the digest
		// of one image with the manifest of another: what is recorded is true of the digest
		// it is recorded under.
		pinned, err := images.pin(ctx, image, first[image])
		if err != nil {
			fmt.Fprintf(e.Err, "%s is recorded nowhere: %v\n", image, err)
			continue
		}
		read, err := images.readAt(ctx, pinned, first[image])
		if err != nil {
			fmt.Fprintf(e.Err, "%s is recorded nowhere: %v\n", image, err)
			continue
		}
		record.Pins[image] = pinned
		record.Manifests[pinned] = read
	}
	if len(record.Pins)+len(record.Manifests) == 0 {
		return
	}
	if err := recordImages(ctx, at, namespace, record); err != nil {
		fmt.Fprintf(e.Err, "what was read of the images was not recorded in %s: %v\n", namespace, err)
		return
	}
	fmt.Fprintln(e.Out, recorded(record, namespace+" on "+at.base))
}
