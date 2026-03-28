FROM alpine:3.20

RUN apk add --no-cache tor

COPY torrc /etc/tor/torrc

USER tor

ENTRYPOINT ["tor", "-f", "/etc/tor/torrc"]
