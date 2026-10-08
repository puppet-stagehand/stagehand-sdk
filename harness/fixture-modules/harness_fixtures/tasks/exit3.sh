#!/bin/sh
# Bolt fixture task: prints one JSON line and then fails with exit code 3, so
# the harness can record how a real Bolt reports a task that ran and failed.
printf '{"note": "about to fail"}\n'
exit 3
