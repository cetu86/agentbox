# Agent image: git + Node.js + an agent CLI on top of BASE.
# The "playwright" toolchain makes agentbox build on the matching
# mcr.microsoft.com/playwright:v<version>-noble image (headless browsers
# included). Pick BASE to match your host distro (so host-built native deps
# still load), e.g.:
#   agentbox build --base mcr.microsoft.com/playwright:v1.49.0-noble
ARG BASE=ubuntu:24.04
FROM ${BASE}
USER root

RUN apt-get update \
 && apt-get install -y git openssh-client ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

# The Playwright images already ship Node.js; plain bases get Node.js 22.
RUN command -v npm >/dev/null \
 || (curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
     && apt-get install -y nodejs \
     && rm -rf /var/lib/apt/lists/*)

# Agent CLI (change for claude / codex / etc., and update agent_cmd in the config).
RUN npm install -g @github/copilot

# Agents often call `python`; expose it when the base ships python3.
RUN if command -v python3 >/dev/null && ! command -v python >/dev/null; then \
      ln -s "$(command -v python3)" /usr/local/bin/python; \
    fi

ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
# Containers run as your host uid; this keeps git from complaining about ownership.
RUN git config --system --add safe.directory '*'
