# evalsi-demo-dsh: DeepSeek Harness (dsh), for the headless CLI-agent runs and
# `dsh web` as the studio stand-in. Build from this directory:
#   docker build -t evalsi-demo-dsh .
#
# dsh is a developer preview whose behaviour changes between releases, so the
# version is pinned. 0.2.1-alpha.2 is the npm release of commit
# d743267388641bc76f17c45ce8b4c231aed1d32c of deepseek-ai/deepseek-harness.
FROM node:22-slim
ARG DSH_VERSION=0.2.1-alpha.2
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates python3 \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g "@deepseek-ai/dsh@${DSH_VERSION}" \
    && npm cache clean --force
COPY configure.sh /usr/local/bin/dsh-demo-configure
RUN chmod +x /usr/local/bin/dsh-demo-configure
# A non-root user (the pod rung and Pod Security "restricted" need one) with
# a home for dsh's state.
# /workspace is a task's workdir in a sandbox: on the pod rung the image's
# own user creates it, which it cannot do at the filesystem root.
RUN useradd --uid 10001 --create-home demo \
    && mkdir /workspace && chown 10001:10001 /workspace
USER 10001
ENV HOME=/home/demo DSH_HOME=/home/demo/.dsh
WORKDIR /home/demo
# The model comes from the environment at start: see configure.sh.
ENTRYPOINT ["/usr/local/bin/dsh-demo-configure"]
# dsh web refuses a wildcard address: give it the pod's with --host.
CMD ["dsh", "--profile", "web", "--no-open", "--port", "8080"]
