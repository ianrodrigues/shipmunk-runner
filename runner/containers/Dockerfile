FROM node:22-bookworm-slim@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5
RUN apt-get update \
    && apt-get install --yes --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*
RUN npm install --global @openai/codex@0.154.0 @anthropic-ai/claude-code@2.1.269 \
    && npm cache clean --force \
    && mkdir -p /profile /empty \
    && chmod 755 /profile /empty
COPY runner/containers/codex-mcp.mjs runner/containers/codex-result.schema.json /usr/local/lib/shipmunk/
WORKDIR /empty
ENTRYPOINT ["/usr/bin/env"]
