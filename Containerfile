# Agent image: Playwright + headless browsers + git + an agent CLI.
# Pick BASE to match your host distro (so host-built native deps still load)
# and your project's Playwright version, e.g.:
#   agentbox build --base mcr.microsoft.com/playwright:v1.49.0-noble
ARG BASE=mcr.microsoft.com/playwright:v1.49.0-noble
FROM ${BASE}
USER root

RUN apt-get update \
 && apt-get install -y --no-install-recommends git openssh-client ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

# Agent CLI (change for claude / codex / etc., and update agent_cmd in the config).
RUN npm install -g @github/copilot

ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
# Containers run as your host uid; this keeps git from complaining about ownership.
RUN git config --system --add safe.directory '*'
