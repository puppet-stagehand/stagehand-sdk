#!/bin/sh
# Bolt fixture task that does NOT declare supports_noop. If Bolt ever runs it
# under --noop the output below proves the gate failed.
printf '{"ran": true}\n'
