FROM alpine:3.21
RUN adduser -D -u 65532 nonroot
COPY brick.yaml /agk/brick.yaml
COPY emit.sh /usr/local/bin/emit
RUN chmod 0755 /usr/local/bin/emit
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/emit"]
