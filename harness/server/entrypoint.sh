#!/usr/bin/env bash
# Entrypoint of the harness Puppet Server 9 image.
#
# First start: configure the certname, create the CA, generate the two client
# certificates the harness uses (runner.test, which may flush the environment
# cache, and other.test, which may not), copy them into /certs for the runner,
# then run the server in the foreground. It never prints key material.
set -euo pipefail

PUPPET=/opt/puppetlabs/bin/puppet
PUPPETSERVER=/opt/puppetlabs/bin/puppetserver
CERTS=/certs

"$PUPPET" config set certname puppet --section main
"$PUPPET" config set dns_alt_names puppet,localhost --section server

SSLDIR="$("$PUPPET" config print ssldir --section main)"

if [ ! -f "$SSLDIR/ca/ca_crt.pem" ]; then
  "$PUPPETSERVER" ca setup
fi

for name in runner.test other.test; do
  if [ ! -f "$SSLDIR/certs/${name}.pem" ]; then
    "$PUPPETSERVER" ca generate --certname "$name"
  fi
done

mkdir -p "$CERTS"
install -m 0644 -o 1000 -g 1000 "$SSLDIR/certs/ca.pem" "$CERTS/ca.pem"
for name in runner.test other.test; do
  install -m 0644 -o 1000 -g 1000 "$SSLDIR/certs/${name}.pem" "$CERTS/${name}.pem"
  install -m 0640 -o 1000 -g 1000 "$SSLDIR/private_keys/${name}.key" "$CERTS/${name}.key"
done

# The service account owns what the server reads and writes.
chown -R puppet:puppet "$SSLDIR"

exec runuser -u puppet -- "$PUPPETSERVER" foreground
