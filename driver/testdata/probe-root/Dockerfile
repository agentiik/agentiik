FROM alpine:3.21
COPY brick.yaml /agk/brick.yaml
COPY entrypoint.sh /usr/local/bin/probe
RUN chmod 0755 /usr/local/bin/probe
ENTRYPOINT ["/usr/local/bin/probe"]
