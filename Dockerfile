FROM alpine:latest

COPY ./build/dnsdock /
RUN chmod a+x /dnsdock

ENTRYPOINT ["/dnsdock"]
