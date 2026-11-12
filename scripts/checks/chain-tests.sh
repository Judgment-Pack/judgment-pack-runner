#!/usr/bin/env bash
# CI step: Chain, signature, binding and ownership tests run under the pinned Runtimes
# Selected Core0.2/Core0.3 use the separate retained-column companion; the
# ordinary and earlier Runtime pins remain unchanged. Both lifecycle tests must pass.
# Ordinary mapped-source runs of Core 0.2.0-draft and 0.3.0 packs side by side
# use the same companion, which evaluates Core 0.3.0 (#188).
# The dialect document comparison with the companion's document tools (#86)
# must run and pass too, and so must the regeneration of a sample of the
# Core 0.3.0 recording set through the same companion (#174).
# So must the release checks held to a supplied reviewed set through the
# companion with `packs covers` (#210).
# Ordinary runs of Core 0.2.0-draft, 0.3.0 and 0.5.0 packs side by side use
# the companion that reads and evaluates Core 0.5.0, and a 0.5.0 pack must be
# refused by the tested Runtime and the retained-column companion, which do
# not say they evaluate it (#232).
# A skipped test passes `go test`; these must run (#35, #54, #55).
set -e
root=$(git rev-parse --show-toplevel)
bin=$root/.ci/bin
out="$(JPACK_TEST_BIN="$bin/jpack" \
  JPACK_RETAINED_TEST_BIN="$bin/jpack-retained" \
  JPACK_COVERS_TEST_BIN="$bin/jpack-covers" \
  JPACK_CORE050_TEST_BIN="$bin/jpack-core050" \
  JPACK_EARLIER_TEST_BIN="$bin/jpack-earlier" \
  JPACK_GATEWAY_TEST_BIN="$bin/gateway" \
  JPACK_MCP_ADAPTER_TEST_BIN="$bin/adapter-mcp" \
  go test -count=1 -v \
  -run '^(TestTheRuntimesVerifierReadsTheChainOfRuns|TestASignedRunKeepsItsSignature|TestVerifyRunChecksARunTheRuntimeSigned|TestASeedReplacedAfterStartSignsNothing|TestAnEarlierRuntimeSigningWithAReplacedSeedIsNamed|TestAnOperationalEvaluationBindsItsPreparation|TestABindingTheRuntimeRefusesFailsTheRun|TestARecordWithoutTheBindingPassedFailsTheRun|TestARuntimeWithoutTheBindingIsGivenNone|TestVerifyRunHoldsTheRecordsBindingToThePreparation|TestVerifyRunReportsARecordTheRuntimeDidNotBind|TestARuntimeThatDoesNotSayItsReleaseEvaluatesNothing|TestNamedDialectRealRuntimeReleaseRunOfflineAndRestart|TestARunWhoseReceiptSessionIsNoTokenIsEvaluatedExportedAndVerified|TestExecutorOwnedServiceRealRuntimeJobsAndChecks|TestExecutorOwnedRealRuntimeSurvivesOwnerDeath|TestColumnRoleRealRuntimeFirstSaveSignedExports|TestSelectedLeafCore030RealRuntimeFirstSaveSignedExports|TestOrdinaryCoreVersionsRealRuntimeSideBySide|TestDialectDocumentsAgreeWithTheRuntimesDocumentTools|TestDialectDocumentRefusalsAgreeWithTheRuntimesDocumentTools|TestADocumentTheRuntimeAcceptsIsNotExecutableOnThatAlone|TestCore030RecordingSetSampleRegenerates|TestSuppliedReviewedSetAgreesWithTheRuntimesPacksCovers|TestOrdinaryCore050RealRuntimeSideBySide|TestCore050RefusedByARealRuntimeThatDoesNotSayItEvaluatesIt)$' \
  ./internal/runner ./cmd/jpack-runner)" || { printf '%s\n' "$out"; exit 1; }
printf '%s\n' "$out"
for name in TestTheRuntimesVerifierReadsTheChainOfRuns TestASignedRunKeepsItsSignature TestVerifyRunChecksARunTheRuntimeSigned TestASeedReplacedAfterStartSignsNothing TestAnEarlierRuntimeSigningWithAReplacedSeedIsNamed TestAnOperationalEvaluationBindsItsPreparation TestABindingTheRuntimeRefusesFailsTheRun TestARecordWithoutTheBindingPassedFailsTheRun TestARuntimeWithoutTheBindingIsGivenNone TestVerifyRunHoldsTheRecordsBindingToThePreparation TestVerifyRunReportsARecordTheRuntimeDidNotBind TestARuntimeThatDoesNotSayItsReleaseEvaluatesNothing TestNamedDialectRealRuntimeReleaseRunOfflineAndRestart TestNamedDialectRealRuntimeReleaseRunOfflineAndRestart/2 TestNamedDialectRealRuntimeReleaseRunOfflineAndRestart/3 TestARunWhoseReceiptSessionIsNoTokenIsEvaluatedExportedAndVerified TestExecutorOwnedServiceRealRuntimeJobsAndChecks TestColumnRoleRealRuntimeFirstSaveSignedExports TestSelectedLeafCore030RealRuntimeFirstSaveSignedExports TestOrdinaryCoreVersionsRealRuntimeSideBySide TestDialectDocumentsAgreeWithTheRuntimesDocumentTools TestDialectDocumentRefusalsAgreeWithTheRuntimesDocumentTools TestADocumentTheRuntimeAcceptsIsNotExecutableOnThatAlone TestCore030RecordingSetSampleRegenerates TestSuppliedReviewedSetAgreesWithTheRuntimesPacksCovers TestSuppliedReviewedSetAgreesWithTheRuntimesPacksCovers/covered TestSuppliedReviewedSetAgreesWithTheRuntimesPacksCovers/no-entry TestSuppliedReviewedSetAgreesWithTheRuntimesPacksCovers/malformed TestOrdinaryCore050RealRuntimeSideBySide TestCore050RefusedByARealRuntimeThatDoesNotSayItEvaluatesIt TestCore050RefusedByARealRuntimeThatDoesNotSayItEvaluatesIt/JPACK_TEST_BIN TestCore050RefusedByARealRuntimeThatDoesNotSayItEvaluatesIt/JPACK_RETAINED_TEST_BIN; do
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
