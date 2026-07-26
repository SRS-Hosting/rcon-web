FROM scratch

USER 65535:65534

COPY rcon-web /
ENTRYPOINT ["/rcon-web"]
