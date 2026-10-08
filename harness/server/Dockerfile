# syntax=docker/dockerfile:1
#
# Puppet Server 9 for the Stagehand harness (FND-06, D-18).
#
# Puppet Server 9 packages are published only on the authenticated Puppet Core
# apt repository. The login and key reach this build ONLY through BuildKit secret
# mounts (see harness/images.sh build server). They are never a build argument,
# never an ENV line, never echoed, and the auth file is put back to the vendor's
# commented template in the same RUN step that used it, so no layer keeps them.
#
# Build it only with harness/images.sh. Compose never builds this image.

# harness/images.sh always passes SERVER_BASE_IMAGE from harness/versions.env. The
# default repeats that pin (same digest) only so a direct "docker build" can never
# fall back to a floating tag and so the Dockerfile linter sees a valid base name.
ARG SERVER_BASE_IMAGE=ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55
FROM ${SERVER_BASE_IMAGE} AS release

ARG PUPPET_RELEASE_SERIES
ARG PUPPET_RELEASE_DEB

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

# The release package is public. It installs the signed-by keyring, the apt
# source line for the puppet9 component and an auth template (a machine line plus
# commented login and password lines, root-owned, mode 0600; checked 2026-10-08).
RUN curl -fsSL -o /tmp/puppet-release.deb "https://apt-puppetcore.puppet.com/public/${PUPPET_RELEASE_DEB}" \
 && dpkg -i /tmp/puppet-release.deb \
 && rm -f /tmp/puppet-release.deb

FROM release AS server

# One step: write the credential from the secret files, install, restore the
# commented template, clean up. No echo, no set -x, no variable holds a secret.
RUN --mount=type=secret,id=puppetcore_user,required=true \
    --mount=type=secret,id=puppetcore_key,required=true \
    set -eu; \
    auth=/etc/apt/auth.conf.d/apt-puppetcore-puppet.conf; \
    mkdir -p /etc/apt/auth.conf.d; \
    if [ -f "$auth" ]; then cp "$auth" /tmp/auth.template; \
    else printf '%s\n' '# machine apt-puppetcore.puppet.com' '# login <api_user>' '# password <api_key>' > /tmp/auth.template; fi; \
    { printf 'machine apt-puppetcore.puppet.com\nlogin '; cat /run/secrets/puppetcore_user; \
      printf '\npassword '; cat /run/secrets/puppetcore_key; printf '\n'; } > "$auth"; \
    chown root:root "$auth"; chmod 0600 "$auth"; \
    apt-get update; \
    apt-get install -y --no-install-recommends puppetserver; \
    cp /tmp/auth.template "$auth"; chown root:root "$auth"; chmod 0600 "$auth"; \
    rm -f /tmp/auth.template; \
    rm -rf /var/lib/apt/lists/* /var/log/apt /var/log/dpkg.log /var/log/apt/term.log

COPY env-cache-rule.conf /tmp/env-cache-rule.conf

RUN set -eu; \
    mkdir -p /etc/stagehand-harness; \
    { dpkg-query -W -f='${Version}\n' puppetserver; /opt/puppetlabs/bin/puppetserver --version 2>&1 || true; } \
      > /etc/stagehand-harness/puppetserver-version; \
    auth=/etc/puppetlabs/puppetserver/conf.d/auth.conf; \
    cp "$auth" /etc/stagehand-harness/auth.conf.package-default; \
    grep -Eq 'rules:[[:space:]]*\[' "$auth" \
      || { echo "auth.conf has no 'rules: [' anchor: the package layout changed, update harness/server" >&2; exit 1; }; \
    awk 'BEGIN { done = 0 } { print } !done && /rules:[[:space:]]*\[/ { while ((getline line < "/tmp/env-cache-rule.conf") > 0) print line; done = 1 }' \
      "$auth" > /tmp/auth.conf.new; \
    cat /tmp/auth.conf.new > "$auth"; \
    rm -f /tmp/auth.conf.new /tmp/env-cache-rule.conf; \
    touch /etc/default/puppetserver; \
    sed -i '/^JAVA_ARGS=/d' /etc/default/puppetserver; \
    echo 'JAVA_ARGS="-Xms512m -Xmx512m"' >> /etc/default/puppetserver

COPY entrypoint.sh /usr/local/bin/harness-server-entrypoint
RUN chmod 0755 /usr/local/bin/harness-server-entrypoint

EXPOSE 8140

# Emulated amd64 starts slowly, hence the long start period.
HEALTHCHECK --interval=10s --timeout=10s --start-period=180s --retries=6 \
  CMD curl -fsk https://localhost:8140/status/v1/simple | grep -q running

ENTRYPOINT ["/usr/local/bin/harness-server-entrypoint"]
