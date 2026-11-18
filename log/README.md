# Curated experiment reports

This directory keeps short Markdown reports that explain the environment, method, result and limitations of significant historical experiments.

Raw JSON responses, console captures, profiles, generated patches and cluster snapshots are deliberately excluded from the product branch. They previously accounted for 4,983 tracked files and obscured the deployable source. The last repository state containing those raw captures is commit `91f5749`; Git history remains available for audit and recovery.

New large evidence bundles should be attached to a GitHub release or pull request instead of committed here. Small deterministic fixtures needed by automated tests belong next to the corresponding test code.
