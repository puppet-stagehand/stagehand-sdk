#!/bin/sh
# Bolt fixture task that declares supports_noop. Bolt passes the --noop flag to
# the task as the environment variable PT__noop; this prints what it saw.
printf '{"noop": "%s"}\n' "${PT__noop:-unset}"
