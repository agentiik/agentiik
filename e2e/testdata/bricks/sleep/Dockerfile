FROM alpine:3.21
RUN adduser -D -u 65532 nonroot
COPY brick.yaml /agk/brick.yaml
COPY sleep.sh /usr/local/bin/sleep-then-publish
RUN chmod 0755 /usr/local/bin/sleep-then-publish
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/sleep-then-publish"]
