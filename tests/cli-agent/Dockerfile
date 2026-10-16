# Pinned CLI used by the opt-in live kind check; no credentials enter the image.
FROM node:22-bookworm-slim
RUN npm install -g @anthropic-ai/claude-code@2.1.296 \
    && useradd --uid 65532 --create-home evalsi-cli
USER 65532:65532
ENV HOME=/workspace
WORKDIR /workspace
