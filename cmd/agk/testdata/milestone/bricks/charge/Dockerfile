# A plain image, built by the test and pushed nowhere, which is the image a laptop meets.
FROM alpine:3.21
RUN adduser -D -u 65532 nonroot
COPY brick.yaml /agk/brick.yaml
COPY charge.sh /usr/local/bin/charge
RUN chmod 0755 /usr/local/bin/charge
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/charge"]
