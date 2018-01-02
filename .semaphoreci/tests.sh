#!/usr/bin/env bash
set -e

ON_CI=true TEST_HOST=1 TESTFLAGS='-check.f KubernetesSuite.TestManifestExamples' make test-integration
