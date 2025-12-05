FROM alpine:latest

COPY ./build/dnsdock /
COPY entrypoint.sh /
RUN chmod a+x /dnsdock /entrypoint.sh && mkdir -p /entrypoint.d

ENTRYPOINT ["/entrypoint.sh"]
CMD ["/dnsdock"]
