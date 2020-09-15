#!/usr/bin/env bash

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
set -euxo pipefail

pushd enterprise
./cmd/server/pre-build.sh
./cmd/server/build.sh
popd
./dev/ci/e2e.sh
docker image rm -f "${IMAGE}"
