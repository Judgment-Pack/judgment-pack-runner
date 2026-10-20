#!/usr/bin/env bash
# CI step: Manual admission helper and real Runtime gate tests cannot skip
set -e
root=$(git rev-parse --show-toplevel)
bin=$root/.ci/bin
out="$(JPACK_TEST_BIN="$bin/jpack" \
  JPACK_OLD_RUNNER_TEST_BIN="$bin/jpack-runner-old" \
  go test -count=1 -v \
  -run '^(TestManualGovernedRealRuntimeGate|TestManualGovernedActualRuntimeHasNoAuthorityFD|TestManualStorePrepareActualChildRetainsOriginalCustody|TestManualStorePreparationDurableAndNoDowngrade|TestManualStorePreparationSubstitutedStateRefuses|TestManualStoreSQLiteTargetUsesHeldDirectory|TestManualStoreLibraryNeverChangesCWD|TestManualInheritedAuthoritySocketClosesBeforeRuntimeExec|TestManualAuthorityDescriptorObserverDetectsSocket|TestManualStorePreparationPendingAndUnsupported|TestManualExternalDemandCannotBeOmitted|TestManualQueuedRunListsKeepPrivatePreimages)$' \
  ./internal/runner)" || { printf '%s\n' "$out"; exit 1; }
printf '%s\n' "$out"
for name in \
  TestManualGovernedRealRuntimeGate \
  TestManualGovernedRealRuntimeGate/success \
  TestManualGovernedRealRuntimeGate/blocked \
  TestManualGovernedRealRuntimeGate/lost-reply \
  TestManualGovernedRealRuntimeGate/changed-facts \
  TestManualGovernedRealRuntimeGate/changed-config \
  TestManualGovernedRealRuntimeGate/changed-lock \
  TestManualGovernedRealRuntimeGate/changed-signing \
  TestManualGovernedRealRuntimeGate/changed-epoch \
  TestManualGovernedRealRuntimeGate/changed-demand \
  TestManualGovernedRealRuntimeGate/journal-failure \
  TestManualGovernedRealRuntimeGate/start-journal-failure \
  TestManualGovernedRealRuntimeGate/recovered-reserve \
  TestManualStorePrepareActualChildRetainsOriginalCustody \
  TestManualStorePrepareActualChildRetainsOriginalCustody/committed \
  TestManualStorePrepareActualChildRetainsOriginalCustody/refused \
  TestManualStorePrepareActualChildRetainsOriginalCustody/lose-reply \
  TestManualStorePreparationDurableAndNoDowngrade \
  TestManualStorePreparationSubstitutedStateRefuses \
  TestManualStorePreparationSubstitutedStateRefuses/before-adapter \
  TestManualStorePreparationSubstitutedStateRefuses/after-adapter \
  TestManualStoreSQLiteTargetUsesHeldDirectory \
  TestManualStoreLibraryNeverChangesCWD \
  TestManualInheritedAuthoritySocketClosesBeforeRuntimeExec \
  TestManualAuthorityDescriptorObserverDetectsSocket \
  TestManualStorePreparationPendingAndUnsupported \
  TestManualExternalDemandCannotBeOmitted \
  TestManualQueuedRunListsKeepPrivatePreimages; do
  printf '%s\n' "$out" | grep -q -- "^[[:space:]]*--- PASS: $name " || { echo "$name did not run and pass"; exit 1; }
done
# This observer identifies the actual Runtime through Linux /proc.
# Every other named helper/gate test is mandatory on both hosts.
if [ "$(uname -s)" = Linux ]; then
  name=TestManualGovernedActualRuntimeHasNoAuthorityFD
  printf '%s\n' "$out" | grep -q -- "^[[:space:]]*--- PASS: $name " || { echo "$name did not run and pass"; exit 1; }
fi
