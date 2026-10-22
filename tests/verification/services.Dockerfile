FROM scratch
WORKDIR /tmp
COPY horizon-evalsid /usr/local/bin/evalsid
COPY horizon-evalsi-guest /usr/local/bin/evalsi-guest
ENTRYPOINT ["/usr/local/bin/evalsid"]
