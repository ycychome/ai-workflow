#!/bin/sh
set -eu
. "$(dirname "$0")/alpha-common.sh"
# Network access occurs only during trusted image preparation.
docker pull postgres:16-bookworm
docker pull postgres:17
alpha_compose --profile images build runner-image
alpha_compose build control
printf '%s\n' 'Images prepared; no controller or test was started.'
