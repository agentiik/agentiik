FROM alpine:3.21
RUN adduser -D -u 65532 nonroot
COPY brick.yaml /agk/brick.yaml
COPY count.sh /usr/local/bin/count
RUN chmod 0755 /usr/local/bin/count
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/count"]
