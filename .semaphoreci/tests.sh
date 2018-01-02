#!/usr/bin/env bash
set -e

make test-unit
ci_retry ON_CI=true make test-integration
make -j${N_MAKE_JOBS} crossbinary-default-parallel
