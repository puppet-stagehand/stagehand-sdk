#!/usr/bin/env bash
# Entrypoint of the harness Puppet Server 9 image.
#
# First start: configure the certname, create the CA, start the server, ask the
# running CA to sign the two client certificates the harness uses (runner.test,
# which may flush the environment cache, and other.test, which may not), copy
# them into /certs for the runner, then keep the server in the foreground. It
# never prints key material.
#
# Puppet Server 9's `puppetserver ca generate` talks to the running CA over
# HTTPS (the offline form, --ca-client, would hand the certificate CA-API
# rights, which the runner must not have), so the server has to be up first.
set -euo pipefail

PUPPET=/opt/puppetlabs/bin/puppet
PUPPETSERVER=/opt/puppetlabs/bin/puppetserver
CERTS=/certs
READY_TIMEOUT="${HARNESS_SERVER_READY_TIMEOUT:-240}"

"$PUPPET" config set certname puppet --section main
"$PUPPET" config set dns_alt_names puppet,localhost --section server

SSLDIR="$("$PUPPET" config print ssldir --section main)"

if [ ! -f "$SSLDIR/ca/ca_crt.pem" ]; then
  "$PUPPETSERVER" ca setup
fi

# The service account owns what the server reads and writes.
chown -R puppet:puppet "$SSLDIR"

runuser -u puppet -- "$PUPPETSERVER" foreground &
SERVER_PID=$!
trap 'kill -TERM "$SERVER_PID" 2>/dev/null || true' TERM INT

ready=0
for _ in $(seq 1 "$READY_TIMEOUT"); do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "Puppet Server exited before it became ready" >&2
    wait "$SERVER_PID" || true
    exit 1
  fi
  if curl -fsk https://localhost:8140/status/v1/simple 2>/dev/null | grep -q running; then
    ready=1
    break
  fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "Puppet Server was not ready after ${READY_TIMEOUT}s" >&2
  kill -TERM "$SERVER_PID" 2>/dev/null || true
  exit 1
fi

for name in runner.test other.test; do
  if [ ! -f "$SSLDIR/certs/${name}.pem" ]; then
    "$PUPPETSERVER" ca generate --certname "$name" >/dev/null
  fi
done

mkdir -p "$CERTS"
install -m 0644 -o 1000 -g 1000 "$SSLDIR/certs/ca.pem" "$CERTS/ca.pem"
for name in runner.test other.test; do
  install -m 0644 -o 1000 -g 1000 "$SSLDIR/certs/${name}.pem" "$CERTS/${name}.pem"
  install -m 0640 -o 1000 -g 1000 "$SSLDIR/private_keys/${name}.pem" "$CERTS/${name}.key"
done

chown -R puppet:puppet "$SSLDIR"

wait "$SERVER_PID"
