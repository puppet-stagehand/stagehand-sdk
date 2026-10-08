# Real-tool runner for the Stagehand harness (FND-05, FND-06).
#
# Every pin comes from harness/versions.env through compose build args. Nothing
# here runs on a developer machine except inside this image, and the image runs
# as the non-root user "deploy" so permission faults are not hidden by root
# (T-13-21).
ARG RUBY_IMAGE
FROM ${RUBY_IMAGE}

ARG TARGETARCH
ARG R10K_VERSION
ARG BOLT_VERSION
ARG G10K_VERSION
ARG G10K_SHA256_LINUX_AMD64
ARG G10K_SHA256_LINUX_ARM64
ARG HTTP_REQUEST_VERSION

ENV BOLT_GEM=true \
    BOLT_DISABLE_ANALYTICS=true

RUN gem install --no-document r10k -v "${R10K_VERSION}" \
 && gem install --no-document bolt -v "${BOLT_VERSION}"

# g10k is a static binary. The build fails on a checksum mismatch or an
# architecture with no pinned checksum (T-13-SC).
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) want="${G10K_SHA256_LINUX_AMD64}" ;; \
      arm64) want="${G10K_SHA256_LINUX_ARM64}" ;; \
      *) echo "no pinned g10k checksum for architecture '${TARGETARCH}'" >&2; exit 1 ;; \
    esac; \
    url="https://github.com/voxpupuli/g10k/releases/download/v${G10K_VERSION}/g10k_${G10K_VERSION}_linux_${TARGETARCH}.tar.gz"; \
    mkdir -p /tmp/g10k; \
    curl -fsSL -o /tmp/g10k/g10k.tar.gz "${url}"; \
    echo "${want}  /tmp/g10k/g10k.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/g10k/g10k.tar.gz -C /tmp/g10k g10k; \
    install -m 0755 /tmp/g10k/g10k /usr/local/bin/g10k; \
    rm -rf /tmp/g10k

RUN useradd --create-home --uid 1000 --shell /bin/bash deploy \
 && mkdir -p /srv/git /srv/work /opt/stagehand-bolt /opt/fixture-modules /opt/harness-scripts \
 && chown deploy:deploy /srv/git /srv/work /opt/stagehand-bolt

USER deploy
WORKDIR /srv/work

# Bolt project: the fixture modules are mounted at /opt/fixture-modules and the
# Forge module that sends the cache-flush request is installed at build time
# (the Bolt gem bundles no modules).
RUN printf '%s\n' \
      'name: stagehand_harness' \
      'modulepath:' \
      '  - /opt/fixture-modules' \
      'modules:' \
      '  - name: puppetlabs-http_request' \
      "    version_requirement: \"${HTTP_REQUEST_VERSION}\"" \
      > /opt/stagehand-bolt/bolt-project.yaml \
 && bolt module install --project /opt/stagehand-bolt

CMD ["sleep", "infinity"]
