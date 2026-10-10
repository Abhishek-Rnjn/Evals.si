# evalsi-demo-deepagents: the Deep Agents demo agent (HTTP service) and dcode.
# Build from this directory:  docker build -t evalsi-demo-deepagents .
FROM python:3.13-slim
ENV PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 MLFLOW_DISABLE_AGENT_HINT=1
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /opt/demo
COPY requirements.txt .
RUN pip install -r requirements.txt
COPY agent.py server.py ./
COPY corpus ./corpus
# Runs as a non-root user, so it works on the pod rung and under Pod Security
# "restricted"; the user needs a home for dcode's config.
# /workspace is a task's workdir in a sandbox: on the pod rung the image's
# own user creates it, which it cannot do at the filesystem root.
RUN useradd --uid 10001 --create-home demo \
    && mkdir /workspace && chown 10001:10001 /workspace
USER 10001
ENV HOME=/home/demo DEMO_CORPUS=/opt/demo/corpus
EXPOSE 8080
ENTRYPOINT ["python", "/opt/demo/server.py"]
