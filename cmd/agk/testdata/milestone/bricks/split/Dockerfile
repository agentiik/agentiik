# A plain image, built by the test and pushed nowhere, which is the image a laptop meets.
FROM alpine:3.21
RUN adduser -D -u 65532 nonroot
COPY brick.yaml /agk/brick.yaml
COPY split.sh /usr/local/bin/split
RUN chmod 0755 /usr/local/bin/split
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/split"]
