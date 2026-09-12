FROM alpine:3.21
RUN adduser -D -u 65532 nonroot
COPY brick.yaml /agk/brick.yaml
COPY entrypoint.sh /usr/local/bin/probe
RUN chmod 0755 /usr/local/bin/probe
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/probe"]
