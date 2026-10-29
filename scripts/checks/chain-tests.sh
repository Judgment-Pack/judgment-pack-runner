#!/usr/bin/env bash
# CI step: Chain, signature, binding and ownership tests run under the pinned Runtimes
# Selected Core0.2/Core0.3 use the separate retained-column companion; the
# ordinary and earlier Runtime pins remain unchanged. Both lifecycle tests must pass.
# The dialect document comparison with the companion's document tools (#86)
# must run and pass too.
# A skipped test passes `go test`; these must run (#35, #54, #55).
set -e
root=$(git rev-parse --show-toplevel)
bin=$root/.ci/bin
out="$(JPACK_TEST_BIN="$bin/jpack" \
  JPACK_RETAINED_TEST_BIN="$bin/jpack-retained" \
  JPACK_EARLIER_TEST_BIN="$bin/jpack-earlier" \
  JPACK_GATEWAY_TEST_BIN="$bin/gateway" \
  JPACK_MCP_ADAPTER_TEST_BIN="$bin/adapter-mcp" \
  go test -count=1 -v \
  -run '^(TestTheRuntimesVerifierReadsTheChainOfRuns|TestASignedRunKeepsItsSignature|TestVerifyRunChecksARunTheRuntimeSigned|TestASeedReplacedAfterStartSignsNothing|TestAnEarlierRuntimeSigningWithAReplacedSeedIsNamed|TestAnOperationalEvaluationBindsItsPreparation|TestABindingTheRuntimeRefusesFailsTheRun|TestARecordWithoutTheBindingPassedFailsTheRun|TestARuntimeWithoutTheBindingIsGivenNone|TestVerifyRunHoldsTheRecordsBindingToThePreparation|TestVerifyRunReportsARecordTheRuntimeDidNotBind|TestARuntimeThatDoesNotSayItsReleaseEvaluatesNothing|TestNamedDialectRealRuntimeReleaseRunOfflineAndRestart|TestARunWhoseReceiptSessionIsNoTokenIsEvaluatedExportedAndVerified|TestExecutorOwnedServiceRealRuntimeJobsAndChecks|TestExecutorOwnedRealRuntimeSurvivesOwnerDeath|TestColumnRoleRealRuntimeFirstSaveSignedExports|TestSelectedLeafCore030RealRuntimeFirstSaveSignedExports|TestDialectDocumentsAgreeWithTheRuntimesDocumentTools|TestDialectDocumentRefusalsAgreeWithTheRuntimesDocumentTools|TestADocumentTheRuntimeAcceptsIsNotExecutableOnThatAlone)$' \
  ./internal/runner ./cmd/jpack-runner)" || { printf '%s\n' "$out"; exit 1; }
printf '%s\n' "$out"
for name in TestTheRuntimesVerifierReadsTheChainOfRuns TestASignedRunKeepsItsSignature TestVerifyRunChecksARunTheRuntimeSigned TestASeedReplacedAfterStartSignsNothing TestAnEarlierRuntimeSigningWithAReplacedSeedIsNamed TestAnOperationalEvaluationBindsItsPreparation TestABindingTheRuntimeRefusesFailsTheRun TestARecordWithoutTheBindingPassedFailsTheRun TestARuntimeWithoutTheBindingIsGivenNone TestVerifyRunHoldsTheRecordsBindingToThePreparation TestVerifyRunReportsARecordTheRuntimeDidNotBind TestARuntimeThatDoesNotSayItsReleaseEvaluatesNothing TestNamedDialectRealRuntimeReleaseRunOfflineAndRestart TestNamedDialectRealRuntimeReleaseRunOfflineAndRestart/2 TestNamedDialectRealRuntimeReleaseRunOfflineAndRestart/3 TestARunWhoseReceiptSessionIsNoTokenIsEvaluatedExportedAndVerified TestExecutorOwnedServiceRealRuntimeJobsAndChecks TestColumnRoleRealRuntimeFirstSaveSignedExports TestSelectedLeafCore030RealRuntimeFirstSaveSignedExports TestDialectDocumentsAgreeWithTheRuntimesDocumentTools TestDialectDocumentRefusalsAgreeWithTheRuntimesDocumentTools TestADocumentTheRuntimeAcceptsIsNotExecutableOnThatAlone; do
  printf '%s\n' "$out" | grep -q -- "^[[:space:]]*--- PASS: $name " || { echo "$name did not run and pass"; exit 1; }
done
# The exact real-Runtime lifetime observer uses Linux /proc and an
# isolated subreaper. The cross-platform Service test above is required
# on both CI hosts; Linux must also prove both owner-stop boundaries.
if [ "$(uname -s)" = Linux ]; then
  for name in TestExecutorOwnedRealRuntimeSurvivesOwnerDeath TestExecutorOwnedRealRuntimeSurvivesOwnerDeath/close TestExecutorOwnedRealRuntimeSurvivesOwnerDeath/kill; do
    printf '%s\n' "$out" | grep -q -- "^[[:space:]]*--- PASS: $name " || { echo "$name did not run and pass"; exit 1; }
  done
fi
