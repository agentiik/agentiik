// The first agentiik.yaml of an empty repository, which the editor opens on so that a workflow
// created in the console is written in the console too: the documentation's first run, named for the
// workflow and its namespace, one script step that greets and an output reading it.
//
// Its image is pinned by digest, the index alpine:3.21 named when this was written, read from Docker
// Hub on 2026-10-03, since the pre-receive hook refuses an image named by a tag the repository holds
// no pin for, and pinning one takes agk push, which resolves the tag where the image is: a starting
// file the first commit refuses would be no start at all. The tag stays beside the digest for the
// person reading it, and is not what runs.
export const firstImage = "alpine:3.21@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507";

export function starting(namespace: string, workflow: string): string {
  return `apiVersion: agentiik.dev/v1
kind: Workflow

metadata:
  name: ${workflow}
  namespace: ${namespace}

outputs:
  greeting:
    from: { step: greet, port: out }

steps:
  greet:
    image: ${firstImage}
    script:
      - echo "hello from \${AGK_STEP}"
    outputs: [out]
`;
}
