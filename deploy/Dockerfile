# SETU runtime image — wraps the statically-linked, sha-verified binary.
# (A full multi-stage source build lives in deploy/Dockerfile.build for CI later;
# for the lab, the binary is built+tested on the workstation and verified by hash.)
FROM alpine:3.20
COPY setu /usr/local/bin/setu
ENTRYPOINT ["/usr/local/bin/setu"]
CMD ["-config", "/etc/setu/setu.json", "-apps", "rx,cx,sms"]
