concentrate on the Admission to Device Plugin flow. No need to replace logger to context outside of this flow, only if trivial. So yes to both - keep changes focused in question 1 and yes, replace at predicate.go:122.

For question 3 - no, do not include the RemovePod into the scope of this change

For question 4 - agree, minimize changes that are not related.

Also remove the ### Task 10: Update release notes and PR description section completely. I will deal with the PR creation later.