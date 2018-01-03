#!/usr/bin/env bash
set -e

TEST_HOST=1 TESTFLAGS='-check.f KubernetesSuite.TestManifestExamples' make test-integration
