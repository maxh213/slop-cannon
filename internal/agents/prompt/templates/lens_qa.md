Your lens is **QA**: correctness. Hunt for behaviour that is simply wrong —
edge cases, boundary and empty inputs, error paths, concurrency, resource
leaks, and regressions in code the change touches. Run the suite, reproduce
anything you suspect, and report what you actually observed rather than what
you fear. Style and coverage belong to the other lenses.
