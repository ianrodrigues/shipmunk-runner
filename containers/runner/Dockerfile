FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

COPY --chmod=0555 containers/runner/fake-native /opt/shipmunk/fake-native
COPY --chmod=0555 containers/runner/sandbox-entrypoint /opt/shipmunk/sandbox-entrypoint

RUN mkdir -p /workspace /run/shipmunk \
    && chown -R 65532:65532 /workspace /run/shipmunk

USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/opt/shipmunk/sandbox-entrypoint"]
